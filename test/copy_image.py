#!/usr/bin/env python3
"""Signed images in a local registry, for test/run.sh.

  copy_image.py copy SOURCE TAG DESTINATION
      copies ghcr.io/SOURCE:TAG (public) with its Sigstore attestations
      (the referrers tag sha256-<hex>) to DESTINATION (host:port/name), by
      digest, so the signatures stay valid.
  copy_image.py attach DESTINATION FROM TO
      attaches the attestation of the tag FROM to the tag TO in
      DESTINATION: a signature for another image.
"""
import hashlib
import json
import sys
import urllib.error
import urllib.request

INDEX = "application/vnd.oci.image.index.v1+json"
MANIFEST = "application/vnd.oci.image.manifest.v1+json"
ACCEPT = ",".join([INDEX, MANIFEST, "application/vnd.docker.distribution.manifest.list.v2+json",
                   "application/vnd.docker.distribution.manifest.v2+json"])


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


plain = urllib.request.build_opener(NoRedirect)


def call(url, method="GET", data=None, headers=None):
    """Body and headers; follows redirects without the Authorization header."""
    request = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with plain.open(request, timeout=120) as response:
            return response.read(), response.headers
    except urllib.error.HTTPError as error:
        if error.code in (301, 302, 303, 307, 308):
            with urllib.request.urlopen(error.headers["Location"], timeout=120) as response:
                return response.read(), response.headers
        raise


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


class Registry:
    def __init__(self, base, token=None):
        self.base = base
        self.auth = {"Authorization": "Bearer " + token} if token else {}

    def manifest(self, reference):
        try:
            body, headers = call(f"{self.base}/manifests/{reference}", headers={**self.auth, "Accept": ACCEPT})
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return None
            raise
        return body

    def blob(self, reference):
        return call(f"{self.base}/blobs/{reference}", headers=self.auth)[0]

    def has_blob(self, reference):
        try:
            call(f"{self.base}/blobs/{reference}", method="HEAD")
            return True
        except urllib.error.HTTPError:
            return False

    def put_blob(self, data):
        _, headers = call(f"{self.base}/blobs/uploads/", method="POST", data=b"")
        location = headers["Location"]
        if location.startswith("/"):
            location = self.base.split("/v2/")[0] + location
        separator = "&" if "?" in location else "?"
        call(f"{location}{separator}digest={digest(data)}", method="PUT", data=data,
             headers={"Content-Type": "application/octet-stream"})

    def put_manifest(self, reference, body):
        media_type = json.loads(body).get("mediaType", MANIFEST)
        call(f"{self.base}/manifests/{reference}", method="PUT", data=body, headers={"Content-Type": media_type})


def copy(source, destination, reference):
    """Copies a manifest with everything it points to; answers its digest."""
    body = source.manifest(reference)
    if body is None:
        raise SystemExit(f"{reference} is not in the source registry")
    value = json.loads(body)
    for child in value.get("manifests", []):
        copy(source, destination, child["digest"])
    for blob in ([value["config"]] if "config" in value else []) + value.get("layers", []):
        if not destination.has_blob(blob["digest"]):
            destination.put_blob(source.blob(blob["digest"]))
    destination.put_manifest(reference, body)
    return digest(body)


def local(name):
    host, _, repository = name.partition("/")
    return Registry(f"http://{host}/v2/{repository}")


def main(args):
    if len(args) == 4 and args[0] == "copy":
        _, repository, tag, target = args
        token = json.loads(call(f"https://ghcr.io/token?scope=repository:{repository}:pull&service=ghcr.io")[0])["token"]
        source = Registry(f"https://ghcr.io/v2/{repository}", token)
        destination = local(target)
        image = copy(source, destination, tag)
        referrers = "sha256-" + image.split(":")[1]
        if source.manifest(referrers) is not None:
            copy(source, destination, referrers)
        print(image)
    elif len(args) == 4 and args[0] == "attach":
        _, target, origin, tag = args
        registry = local(target)
        signed = digest(registry.manifest(origin))
        image = digest(registry.manifest(tag))
        referrers = json.loads(registry.manifest("sha256-" + signed.split(":")[1]))
        attestation = json.loads(registry.manifest(referrers["manifests"][0]["digest"]))
        attestation["subject"]["digest"] = image
        body = json.dumps(attestation).encode()
        registry.put_manifest(digest(body), body)
        entry = dict(referrers["manifests"][0], digest=digest(body), size=len(body))
        registry.put_manifest("sha256-" + image.split(":")[1], json.dumps(dict(referrers, manifests=[entry])).encode())
        print(image)
    else:
        raise SystemExit(__doc__)


if __name__ == "__main__":
    main(sys.argv[1:])

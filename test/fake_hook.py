"""A deploy link for the tests: records each POST, answers with the status in the file 'status'."""
import http.server
import pathlib
import sys

state = pathlib.Path(sys.argv[2])


class Hook(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        status = int((state / "status").read_text() or "200")
        with (state / "calls").open("a") as calls:
            calls.write(f"{self.path} {self.headers.get('Authorization', '-')}\n")
        self.send_response(status)
        self.end_headers()
        self.wfile.write(b'{"message": "queued"}' if status < 300 else b'{"message": "Unauthenticated."}')

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Hook).serve_forever()

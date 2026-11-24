#!/usr/bin/env bash
# trace-why-smoke.sh는 edc trace why가 느린 DNS 질의와 느린 외부 HTTP 호출을 그 요청의 경로 아래에 붙이는지 실제 수집으로
# 확인한다. 단위 시험은 만들어 둔 기록만 읽으므로, kernel이 남기는 PID, 시각, 지연이 trace 사이에서 맞는지는 여기서만 본다.
# 저장소 root에서 ./bin/edc를 빌드한 뒤 Linux에서 실행한다. root가 아니면 edc와 53번 port를 여는 DNS 서버만 sudo로 띄운다.

set -eu

edc=./bin/edc
backend_port=18080
external_port=18081
dns_address=127.0.10.1
work=$(mktemp -d)
record="$work/record"
pids=

sudo=
if [ "$(id -u)" -ne 0 ]; then sudo=sudo; fi

finish() {
	status=$?
	for pid in $pids; do $sudo kill "$pid" 2>/dev/null || true; done
	if [ "$status" -ne 0 ]; then
		echo "--- trace why output"
		cat "$work/why.out" 2>/dev/null || true
		echo "--- trace why stderr"
		cat "$work/why.err" 2>/dev/null || true
		for file in $($sudo sh -c "ls $record/*.err" 2>/dev/null || true); do
			echo "--- $file"
			$sudo cat "$file" || true
		done
	fi
	$sudo rm -rf "$work"
}
trap finish EXIT

fail() {
	echo "trace-why-smoke: $1" >&2
	exit 1
}

# slow.로 시작하는 이름은 0.3초 뒤에 답한다. edc trace dns는 53번 port만 보므로 loopback의 다른 주소에 연다.
cat >"$work/dns.py" <<'EOF'
import socket, struct, sys, threading, time

def answer(sock, query, peer):
    end = 12
    while query[end]:
        end += 1 + query[end]
    end += 5
    if query[12:17] == b"\x04slow":
        time.sleep(0.3)
    sock.sendto(query[:2] + struct.pack(">HHHHH", 0x8180, 1, 1, 0, 0) + query[12:end]
                + b"\xc0\x0c" + struct.pack(">HHIH", 1, 1, 5, 4) + socket.inet_aton("127.0.0.1"), peer)

sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.bind((sys.argv[1], 53))
while True:
    query, peer = sock.recvfrom(512)
    threading.Thread(target=answer, args=(sock, query, peer), daemon=True).start()
EOF

cat >"$work/external.py" <<'EOF'
import http.server, sys, time

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        time.sleep(0.3)
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")
    def log_message(self, *args):
        pass

http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
EOF

# backend는 요청을 하나씩 처리한다. 그래서 요청마다 그 안의 호출이 하나의 요청에만 들어가고, 가릴 수 없는 호출이 없어야 한다.
cat >"$work/backend.py" <<'EOF'
import http.client, http.server, socket, struct, sys

DNS, EXTERNAL = sys.argv[2], int(sys.argv[3])

def resolve(name):
    question = b"".join(bytes([len(label)]) + label.encode() for label in name.split(".")) + b"\x00" + struct.pack(">HH", 1, 1)
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
        sock.settimeout(5)
        sock.sendto(struct.pack(">HHHHHH", 0x1234, 0x0100, 1, 0, 0, 0) + question, (DNS, 53))
        sock.recvfrom(512)

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/dns":
            resolve("slow.edc-why.test")
        elif self.path == "/ext":
            connection = http.client.HTTPConnection("127.0.0.1", EXTERNAL, timeout=5)
            connection.request("GET", "/slow")
            connection.getresponse().read()
            connection.close()
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")
    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
EOF

$sudo python3 "$work/dns.py" "$dns_address" >"$work/dns.log" 2>&1 &
pids="$pids $!"
python3 "$work/external.py" "$external_port" >"$work/external.log" 2>&1 &
external=$!
python3 "$work/backend.py" "$backend_port" "$dns_address" "$external_port" >"$work/backend.log" 2>&1 &
backend=$!
pids="$pids $external $backend"
for attempt in $(seq 50); do
	if curl -s -o /dev/null "http://127.0.0.1:$backend_port/dns" && curl -s -o /dev/null "http://127.0.0.1:$backend_port/ext"; then break; fi
	if [ "$attempt" -eq 50 ]; then fail "the backend did not answer /dns and /ext"; fi
	sleep 0.2
done
# port를 다른 program이 쓰고 있으면 그 program이 대신 답해서 위의 확인을 지나간다.
kill -0 "$backend" 2>/dev/null || fail "the backend exited: $(cat "$work/backend.log")"
kill -0 "$external" 2>/dev/null || fail "the external server exited: $(cat "$work/external.log")"
if [ -s "$work/dns.log" ]; then fail "the DNS server failed: $(cat "$work/dns.log")"; fi

# sudo로 띄운 process에는 kill -0을 보낼 수 없어서, 끝난 것과 exit code를 파일로 알린다.
(
	code=0
	$sudo env EDC_LANG=en NO_COLOR=1 "$edc" trace why --duration 25s --port "$backend_port" --out "$record" >"$work/why.out" 2>"$work/why.err" || code=$?
	echo "$code" >"$work/why.code"
) &
trace=$!

# trace마다 붙는 때가 다르고 http는 TLS 대상을 찾느라 몇 초 늦다. 기록이 끝날 때까지 같은 요청을 계속 보낸다.
requests=0
while [ ! -e "$work/why.code" ]; do
	for path in /dns /ext /ok; do
		curl -s -o /dev/null "http://127.0.0.1:$backend_port$path" || true
	done
	requests=$((requests + 1))
done
wait "$trace"
code=$(cat "$work/why.code")
if [ "$code" -ne 0 ]; then fail "trace why exited with $code"; fi
cat "$work/why.out"

# share는 경로 행 아래에서 label로 시작하는 호출 행의 SHARE 칸이다. 그 행이 없으면 비어 있다.
share() {
	awk -F'\t' -v request="GET $1" -v label="  $2" '
		$1 != "" { inside = ($2 == request) }
		inside && $1 == "" && index($2, label) == 1 { sub("%", "", $6); print $6; exit }
	' "$work/why.out"
}
# calls는 경로 행 아래의 행 수에서 self 행을 뺀 값이다.
calls() {
	awk -F'\t' -v request="GET $1" '
		$1 != "" { inside = ($2 == request) }
		inside && $1 == "" && $2 != "" && $2 !~ /^  self/ { count++ }
		END { print count + 0 }
	' "$work/why.out"
}

dns_share=$(share /dns "dns slow.edc-why.test A")
ext_share=$(share /ext "http GET 127.0.0.1:$external_port/slow")
[ -n "$dns_share" ] && [ "$dns_share" -ge 90 ] || fail "the DNS query is ${dns_share:-missing} percent of GET /dns, want 90 or more"
[ -n "$ext_share" ] && [ "$ext_share" -ge 90 ] || fail "the outgoing request is ${ext_share:-missing} percent of GET /ext, want 90 or more"
grep -q "	GET /ok	" "$work/why.out" || fail "no GET /ok row"
[ "$(calls /ok)" -eq 0 ] || fail "GET /ok has call rows"
if grep -qi "ambiguous" "$work/why.out"; then fail "a server that handles one request at a time has ambiguous calls"; fi
if grep -q "	GET /slow	" "$work/why.out"; then fail "--port did not leave out the requests that the external server received"; fi

# trace show는 같은 기록으로 같은 분석을 낸다. trace why는 그 뒤에 빈 줄과 저장 위치를 더 쓴다.
$sudo env EDC_LANG=en NO_COLOR=1 "$edc" trace show "$record" >"$work/show.out"
head -n -2 "$work/why.out" | diff - "$work/show.out" || fail "trace show printed a different breakdown"
echo "trace-why-smoke: ok after $requests request rounds (dns $dns_share%, ext $ext_share%)"

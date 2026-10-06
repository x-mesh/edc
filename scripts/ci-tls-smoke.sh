#!/usr/bin/env bash
# ci-tls-smoke.sh는 edc trace http --tls가 curl의 HTTPS 요청 평문을 읽는지 확인한다. CI의 tls-smoke job이 부른다.
# 저장소 root에서 ./bin/edc를 빌드한 뒤 실행한다. root가 아니면 edc만 sudo로 띄운다.

set -eu

edc=./bin/edc
port=8443
path="/edc-tls-smoke-${GITHUB_RUN_ID:-local}-$$"
work=$(mktemp -d)
out="$work/out.jsonl"
server=

finish() {
	status=$?
	if [ -n "$server" ]; then kill "$server" 2>/dev/null || true; fi
	if [ "$status" -ne 0 ]; then
		echo "--- out.jsonl"
		cat "$out" 2>/dev/null || true
		echo "--- trace stderr"
		cat "$work/trace.err" 2>/dev/null || true
		echo "--- s_server log"
		cat "$work/server.log" 2>/dev/null || true
	fi
	rm -rf "$work"
}
trap finish EXIT

fail() {
	echo "ci-tls-smoke: $1" >&2
	exit 1
}

captured() {
	grep -F '"event":"http_request"' "$out" | grep -F "\"path\":\"$path\"" | grep -qF '"tls":true'
}

# 인터넷에 기대지 않도록 로컬 서버를 쓴다. edc는 curl 쪽 libssl을 보므로 서버가 평문을 어떻게 읽는지는 상관없다.
openssl req -x509 -newkey rsa:2048 -nodes -subj /CN=localhost -days 1 \
	-keyout "$work/key.pem" -out "$work/cert.pem" 2>/dev/null
openssl s_server -quiet -www -accept "$port" -cert "$work/cert.pem" -key "$work/key.pem" >"$work/server.log" 2>&1 &
server=$!
for attempt in $(seq 50); do
	if curl -sk -o /dev/null "https://127.0.0.1:$port/"; then break; fi
	if [ "$attempt" -eq 50 ]; then fail "s_server did not listen on $port"; fi
	sleep 0.2
done

# sudo로 띄운 process에는 kill -0을 보낼 수 없어서, 끝난 것과 exit code를 파일로 알린다.
sudo=
if [ "$(id -u)" -ne 0 ]; then sudo=sudo; fi
(
	code=0
	$sudo env EDC_LANG=en "$edc" trace http --tls --raw --duration 20s >"$out" 2>"$work/trace.err" || code=$?
	echo "$code" >"$work/trace.code"
) &
trace=$!

for attempt in $(seq 150); do
	if grep -qF -- '--tls: watching' "$work/trace.err" 2>/dev/null; then break; fi
	if [ -e "$work/trace.code" ]; then fail "trace exited before it attached"; fi
	if [ "$attempt" -eq 150 ]; then fail "no --tls: watching line in 15s"; fi
	sleep 0.1
done

# --tls: watching은 BPF를 붙이기 전에 찍힌다. 로컬에서도 그 뒤 1초쯤 요청을 놓쳤으므로 잡힐 때까지 같은 요청을 다시 보낸다.
for attempt in $(seq 10); do
	curl -sk --http1.1 -o /dev/null "https://127.0.0.1:$port$path" || true
	sleep 1
	if captured; then
		echo "captured $path after $attempt request(s)"
		break
	fi
done

wait "$trace"
code=$(cat "$work/trace.code")
if [ "$code" -ne 0 ]; then fail "trace exited with $code"; fi
captured || fail "no http_request event with path $path and tls true"
cat "$work/trace.err"
grep -F "\"path\":\"$path\"" "$out"

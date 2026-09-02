# edc tcp 데모: 연결 성공과 timeout 실패
# 렌더: make build && vhs docs/tape/tcp.tape

Output docs/media/tcp.gif

Source docs/tape/_settings.tape
Set Height 340

Source docs/tape/_setup.tape

Type "edc tcp check example.com:443"
Sleep 400ms
Enter
Wait+Screen /pass/
Sleep 2200ms

# 닫힌 port는 timeout phase를 남기고 exit 1이 됩니다.
Type "edc tcp check --timeout 3s example.com:9999"
Sleep 400ms
Enter
Wait+Screen /1 fail/
Sleep 3s

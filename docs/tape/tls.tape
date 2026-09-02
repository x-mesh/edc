# edc tls 데모: 인증서 확인과 --min-days 임계값
# 렌더: make build && vhs docs/tape/tls.tape

Output docs/media/tls.gif

Source docs/tape/_settings.tape
Set Height 340

Source docs/tape/_setup.tape

Type "edc tls check example.com:443"
Sleep 400ms
Enter
Wait+Screen /pass/
Sleep 2200ms

# 남은 일수가 기준보다 짧으면 warning이 아니라 fail이 됩니다.
Type "edc tls check --min-days 90 example.com:443"
Sleep 400ms
Enter
Wait+Screen /1 fail/
Sleep 3s

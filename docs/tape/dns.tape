# edc dns 데모: 이름 조회와 resolver 설정 확인
# 렌더: make build && vhs docs/tape/dns.tape

Output docs/media/dns.gif

Source docs/tape/_settings.tape
Set Height 270

Source docs/tape/_setup.tape

Type "edc dns lookup example.com"
Sleep 400ms
Enter
Wait+Screen /pass/
Sleep 2200ms

Type "edc dns config"
Sleep 400ms
Enter
Wait+Screen /dns.config/
Sleep 2500ms

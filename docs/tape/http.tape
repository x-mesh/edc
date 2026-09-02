# edc http 데모: 응답 확인과 --expect-status 임계값
# 렌더: make build && vhs docs/tape/http.tape

Output docs/media/http.gif

Source docs/tape/_settings.tape
Set Height 340

Source docs/tape/_setup.tape

Type "edc http check https://example.com"
Sleep 400ms
Enter
Wait+Screen /pass/
Sleep 2200ms

# 기대한 code와 다르면 fail로 처리합니다.
Type "edc http check --expect-status 404 https://example.com"
Sleep 400ms
Enter
Wait+Screen /1 fail/
Sleep 3s

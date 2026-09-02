# edc report 데모: report 뷰어와 diff
# 렌더: make build && vhs docs/tape/report.tape

Output docs/media/report.gif

Source docs/tape/_settings.tape
Set Height 540

Source docs/tape/_setup.tape

# 비교할 report 두 개를 미리 만듭니다. 이 구간은 녹화에 나타나지 않습니다.
Hide
Set TypingSpeed 1ms
# --json <path>는 terminal에 아무것도 쓰지 않으므로 Wait 대신 Sleep으로 기다립니다.
Type "edc doctor --json before.json https://example.com"
Enter
Sleep 8s
Type "edc doctor --json after.json https://example.com"
Enter
Sleep 8s
Type "clear"
Enter
Set TypingSpeed 55ms
Show
Sleep 600ms

Type "edc report show before.json"
Sleep 400ms
Enter
Sleep 2500ms

# f는 필터를 바꾸고 e는 상세를 펼칩니다.
Type "f"
Sleep 1800ms
Type "e"
Sleep 2500ms
Type "q"
Sleep 1500ms

Type "edc report diff before.json after.json"
Sleep 400ms
Enter
Sleep 2500ms

# diff 화면에서도 e가 probe별 metric 차이를 펼칩니다.
Type "e"
Sleep 3s
Type "q"
Sleep 1500ms

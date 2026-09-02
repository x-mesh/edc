# edc doctor 데모: 기본 profile 종합 진단
# 렌더: make build && vhs docs/tape/doctor.tape

Output docs/media/doctor.gif

Source docs/tape/_settings.tape
Set Height 340

Source docs/tape/_setup.tape

Type "edc doctor https://example.com"
Sleep 400ms
Enter
Wait+Screen /9 pass/
Sleep 3500ms

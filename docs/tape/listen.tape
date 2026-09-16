# edc listen 데모: 열려 있는 port 확인
#
# 주의: 이 tape은 실행하는 맥의 정보를 화면에 남기므로 README에 넣지 않습니다.
# 사용자명, 실행 중인 program 이름, 열린 port가 그대로 나옵니다.
# 직접 확인할 때만 렌더하고, 공개 문서에 넣기 전에 화면을 검토하십시오.
# 렌더: make build && vhs docs/tape/listen.tape

Output docs/media/listen.gif

Source docs/tape/_settings.tape
Set Height 1150

Source docs/tape/_setup.tape

Type "edc listen"
Sleep 400ms
Enter
Wait+Screen /pass/
Sleep 2200ms

# --all은 unix domain socket까지 함께 보여 줍니다.
Type "edc listen --all"
Sleep 400ms
Enter
Wait+Screen /unix/
Sleep 3500ms

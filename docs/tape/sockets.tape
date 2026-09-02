# edc sockets 데모: 열린 socket 확인
#
# 주의: 이 tape은 실행하는 맥의 정보를 화면에 남기므로 README에 넣지 않습니다.
# 사용자명, 실행 중인 program 이름, 열린 port가 그대로 나옵니다.
# 직접 확인할 때만 렌더하고, 공개 문서에 넣기 전에 화면을 검토하십시오.
# 렌더: make build && vhs docs/tape/sockets.tape

Output docs/media/sockets.gif

Source docs/tape/_settings.tape
Set Height 700

Source docs/tape/_setup.tape

Type "edc sockets"
Sleep 400ms
Enter
Wait+Screen /pass/
Sleep 2200ms

# -v는 lsof 출력 전체를 상세로 펼칩니다.
Type "edc sockets -v"
Sleep 400ms
Enter
Wait+Screen /output:/
Sleep 3500ms

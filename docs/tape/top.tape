# edc top 데모: 실시간 대시보드와 일시정지
#
# 주의: 이 tape은 실행하는 맥의 정보를 화면에 남기므로 README에 넣지 않습니다.
# hostname과 하드웨어 사양이 그대로 나옵니다.
# 직접 확인할 때만 렌더하고, 공개 문서에 넣기 전에 화면을 검토하십시오.
# 렌더: make build && vhs docs/tape/top.tape

Output docs/media/top.gif

Source docs/tape/_settings.tape
Set Height 380

Source docs/tape/_setup.tape

Type "edc top"
Sleep 400ms
Enter
Sleep 10s

# p는 일시정지입니다. 다시 누르면 재개하고, 첫 행은 멈춘 구간의 평균을 보여 줍니다.
Type "p"
Sleep 2500ms
Type "p"
Sleep 3s

# q로 대시보드를 닫으면 화면에 행이 남지 않습니다.
Type "q"
Sleep 2s

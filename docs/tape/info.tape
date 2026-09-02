# edc info 데모: system, network, disk 정보
#
# 주의: 이 tape은 실행하는 맥의 정보를 화면에 남기므로 README에 넣지 않습니다.
# hostname, 사설 IP와 VPN IP, 업타임, 디스크 용량이 그대로 나옵니다.
# 직접 확인할 때만 렌더하고, 공개 문서에 넣기 전에 화면을 검토하십시오.
# 렌더: make build && vhs docs/tape/info.tape

Output docs/media/info.gif

Source docs/tape/_settings.tape
Set Height 850

Source docs/tape/_setup.tape

Type "edc info"
Sleep 400ms
Enter
Wait+Screen /Disk Usage/
Sleep 4s

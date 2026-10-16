# edc where 데모: 가까운 region과 이 망의 모습
# 렌더: make build && vhs docs/tape/where.tape

# _setup.tape의 redact 설정이 공인 IP와 route 주소를 <ip:…>로 가립니다.

Output docs/media/where.gif

Source docs/tape/_settings.tape
Set Height 960

Source docs/tape/_setup.tape

Type "edc where"
Sleep 400ms
Enter
Wait+Screen@15s /nearest region/
Sleep 3500ms

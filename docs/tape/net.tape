# edc net 데모: interface 목록, route, ping
# 렌더: make build && vhs docs/tape/net.tape

Output docs/media/net.gif

Source docs/tape/_settings.tape
Set Height 340

Source docs/tape/_setup.tape

Type "edc net interfaces"
Sleep 400ms
Enter
Wait+Screen /net.interfaces/
Sleep 1800ms

Type "edc net route example.com"
Sleep 400ms
Enter
Wait+Screen /net.route/
Sleep 1800ms

Type "edc net ping example.com"
Sleep 400ms
Enter
Wait+Screen /net.ping/
# ping은 실시간 화면으로 몇 초 더 돕니다.
Sleep 5s

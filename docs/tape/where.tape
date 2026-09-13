# edc where 데모: 가까운 region과 이 망의 모습
# 렌더: make build && vhs docs/tape/where.tape
#
# edc where는 화면에 주소를 그대로 씁니다. 데모는 공인 IP를 222.XXX.XXX.XXX로 가리고,
# route 주소를 사설 예시 주소 192.168.0.23 → 192.168.0.1로 바꿔 녹화합니다.
# 실제로 누군가 쓰는 주소가 화면에 남지 않게 공인 IP는 숫자 대신 X로 씁니다.
# 치환 래퍼의 script 인자는 macOS 문법입니다.

Output docs/media/where.gif

Source docs/tape/_settings.tape
Set Height 960

Source docs/tape/_setup.tape

# 녹화에 나타나지 않는 구간입니다.
# script로 terminal을 유지해 진행 줄을 그대로 두고, perl은 받은 출력을 바로 치환해 넘깁니다.
Hide
Set TypingSpeed 1ms
Type `edc() { script -q /dev/null "$(type -P edc)" "$@" | perl -e '$|=1; while (sysread(STDIN, $buf, 65536)) { $buf =~ s/^([ \t]*public IP[ \t]+)\S+/${1}222.XXX.XXX.XXX/mg; $buf =~ s/^([ \t]*route[ \t]+\S+[ \t]+)\S+([ \t]+\S+[ \t]+)\S+/${1}192.168.0.23${2}192.168.0.1/mg; print $buf }'; }`
Enter
Type "clear"
Enter
Set TypingSpeed 55ms
Show
Sleep 600ms

Type "edc where"
Sleep 400ms
Enter
Wait+Screen@15s /nearest region/
Sleep 3500ms

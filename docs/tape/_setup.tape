# 녹화에 나타나지 않는 준비 구간입니다.
# 저장소가 빌드한 bin/edc를 PATH 앞에 두고, 빈 임시 디렉터리에서 시작합니다.
# 임시 설정 디렉터리에 redact = true를 두어 IP를 <ip:…>로 가립니다. 녹화한 host의 공인 IP와 내부망 주소가
# 공개 README에 남지 않게 합니다. 사용자의 설정 파일도 읽지 않으므로 기본 언어로 녹화됩니다.

Hide
Set TypingSpeed 1ms
Type 'export PATH=$PWD/bin:$PATH'
Enter
Type 'cd $(mktemp -d)'
Enter
Type 'export XDG_CONFIG_HOME=$PWD/.config && mkdir -p .config/edc && printf "[defaults.common]\nredact = true\n" > .config/edc/config.toml'
Enter
Type "export PS1='$ '"
Enter
Type "clear"
Enter
Set TypingSpeed 55ms
Show
Sleep 600ms

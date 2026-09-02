# edc remote 데모: 실행 전 계획 확인
# 렌더: make build && vhs docs/tape/remote.tape
#
# 데모는 임시 디렉터리에 예제 inventory와 recipe를 만들고 그 안에서 실행합니다.
# SSH 연결과 네트워크 요청은 없습니다.

Output docs/media/remote.gif

Source docs/tape/_settings.tape
Set Height 550

Source docs/tape/_setup.tape

# 데모 데이터를 준비합니다. 이 구간은 녹화에 나타나지 않습니다.
Hide
Set TypingSpeed 1ms
Type "cat > inventory.yaml <<'EOF'"
Enter
Type "hosts:"
Enter
Type "  - name: workstation"
Enter
Type "    tags: [mac]"
Enter
Type "  - name: build-server"
Enter
Type "    tags: [linux]"
Enter
Type "groups:"
Enter
Type "  daily: [workstation, build-server]"
Enter
Type "parallel: 2"
Enter
Type "EOF"
Enter

Type "cat > recipe.yaml <<'EOF'"
Enter
Type "name: daily-update"
Enter
Type "steps:"
Enter
Type "  - name: git-kit"
Enter
Type "    command: git-kit update"
Enter
Type "    verify: git-kit --version"
Enter
Type "  - name: brew"
Enter
Type "    tags: [mac]"
Enter
Type "    command: brew update && brew upgrade"
Enter
Type "  - name: apt"
Enter
Type "    tags: [linux]"
Enter
Type "    command: apt-get update && apt-get -y upgrade"
Enter
Type "EOF"
Enter
Type "clear"
Enter
Set TypingSpeed 55ms
Show
Sleep 600ms

# recipe의 step은 tags로 실행 대상 host를 좁힙니다.
Type "cat recipe.yaml"
Sleep 400ms
Enter
Wait+Screen /daily-update/
Sleep 2500ms

# dry-run은 SSH 연결 없이 host x step 계획만 보여 줍니다.
Type "edc remote daily --dry-run"
Sleep 400ms
Enter
Wait+Screen /build-server/
Sleep 4s

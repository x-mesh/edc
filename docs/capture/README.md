# README 화면 캡처

`top.png`, `info.png`, `trace-http.png`, `history.png`, `disk-usage.png`, `clean.png`는 현재 UI 렌더러에 고정된 데모 데이터를 넣고 VHS terminal에서 캡처한 화면입니다. 실제 호스트를 관측한 결과가 아닙니다. 호스트명, 프로세스, 자원 수치와 문서용 IP 주소는 `frames.go.txt`에서 지정합니다. 실제 호스트의 설정, 계정 정보나 프로세스를 수집하지 않습니다.

저장소 루트에서 실행합니다. Go, Python 3, VHS와 VHS가 사용하는 ttyd·FFmpeg·Chromium이 필요합니다.

```bash
python3 docs/capture/render.py
python3 docs/capture/render.py history disk-usage
```

Go overlay로 캡처 전용 함수를 임시 test 파일에 연결합니다. 제품 코드나 기존 test 파일은 수정하지 않습니다. 함수가 생성한 ANSI 화면을 terminal에서 재생하고 `docs/media/top.png`, `docs/media/info.png`, `docs/media/trace-http.png`, `docs/media/history.png`, `docs/media/disk-usage.png`, `docs/media/clean.png`를 차례로 덮어씁니다. 화면의 `edc top`, `edc info`, `edc trace http`, `edc history trend`, `edc du` 명령은 캡처용 shell 함수로 재생되며 실제 수집기를 실행하지 않습니다. 공통 스타일은 `docs/tape/_settings.tape`를 따릅니다.

`docs/tape/top.tape`와 `info.tape`는 실제 호스트를 녹화하는 별도의 로컬 확인용 tape입니다. 공개 README에는 이 스크립트로 만든 데모 화면을 사용합니다.

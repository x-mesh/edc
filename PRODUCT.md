# Product

## Register

product

## Users

SE와 SRE가 terminal에서 여러 시스템을 진단하고 반복 유지보수 작업을 실행합니다. 사용자는 실행 상태, 실패 원인, 다음 조치를 빠르게 파악해야 합니다.

## Product Purpose

`edc`는 everyday carry의 줄임말입니다. 주머니에 넣고 다니는 도구처럼, SE와 SRE가 terminal에서 가장 먼저 꺼내 쓰는 진단 도구를 지향합니다.

`edc`는 로컬 및 원격 시스템 작업을 일관된 CLI로 실행하고 검증합니다. 성공은 사용자가 작업 진행과 최종 결과를 다시 읽지 않고 이해하는 것입니다.

## Brand Personality

절제됨, 고밀도, 신뢰감. 출력은 정확하고 차분하며 작업 흐름을 방해하지 않습니다.

## Anti-references

과도한 색상, 박스 남발, 장식적 animation, 불필요한 banner, 같은 정보를 반복하는 출력은 사용하지 않습니다.

## Design Principles

- 현재 실행 중인 대상과 단계를 한 줄에서 식별할 수 있게 합니다.
- 색상에만 의존하지 않고 기호와 문구로 상태를 전달합니다.
- 상세 정보는 요청할 때 보여주고 최종 요약은 짧게 유지합니다.
- 병렬 출력에서도 host, step, phase를 일관된 열로 정렬합니다.
- 실패 원인과 후속 영향을 성공 정보보다 먼저 드러냅니다.

## Accessibility & Inclusion

색상을 쓰지 않는 terminal에서도 모든 상태를 구분할 수 있어야 합니다. TTY가 아니거나 `NO_COLOR`가 설정되면 ANSI color와 motion을 사용하지 않습니다.

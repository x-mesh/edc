# 모든 edc 데모가 공유하는 렌더 설정입니다.
# 개별 tape은 이 파일을 Source한 뒤 Width와 Height만 덮어씁니다.
#
# 창 버튼과 둥근 모서리를 단 어두운 창을 흰 바탕 위에 둡니다.
# FontSize 18에서 한 칸은 약 12px, 한 줄은 29px입니다. 창 틀과 여백이 가로 136px, 세로 176px을 씁니다.
# Width 1520은 115칸쯤이라 dns lookup의 가장 긴 줄(108칸)이 줄바꿈 없이 들어갑니다.
# 개별 tape의 Height는 176 + 29 × 보여 줄 줄 수 + 여유 10으로 정합니다.
# 한글은 FiraCode에 없으므로 Sarasa Mono K로 넘깁니다.

Set Shell "bash"
Set FontFamily "FiraCode Nerd Font Mono,Sarasa Mono K"
Set FontSize 18
Set LineHeight 1.25
Set Width 1520
Set Height 540
Set Padding 28
Set WindowBar Colorful
Set WindowBarSize 40
Set BorderRadius 12
Set Margin 40
Set MarginFill "#FFFFFF"
# Catppuccin Mocha 팔레트에서 기본 글자색(foreground, white, brightWhite)만 순백으로 올립니다.
Set Theme { "name": "Mocha White", "black": "#45475a", "red": "#f38ba8", "green": "#a6e3a1", "yellow": "#f9e2af", "blue": "#89b4fa", "magenta": "#f5c2e7", "cyan": "#94e2d5", "white": "#ffffff", "brightBlack": "#585b70", "brightRed": "#f38ba8", "brightGreen": "#a6e3a1", "brightYellow": "#f9e2af", "brightBlue": "#89b4fa", "brightMagenta": "#f5c2e7", "brightCyan": "#94e2d5", "brightWhite": "#ffffff", "background": "#1e1e2e", "foreground": "#ffffff", "selection": "#585b70", "cursor": "#f5e0dc" }
Set TypingSpeed 55ms
Set Framerate 30
Set CursorBlink false

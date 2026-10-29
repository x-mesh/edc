package edc

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

const (
	// aiPaneGap은 표와 세션 영역 사이의 " │ "다.
	aiPaneGap = 3
	// aiPaneMinWidth보다 좁으면 세션 영역을 그리지 않는다. 설명이 몇 글자만 남으면 읽을 수 없다.
	aiPaneMinWidth = 40
)

// aiSessionFeed는 edc ai 대시보드가 세션을 읽는 통로다. edc ai sessions와 같은 증분 읽기와 활동 캐시를 쓴다.
type aiSessionFeed struct {
	tail       *aiEventTail
	activities aiActivityReader
}

type aiSessionFeedMsg struct {
	sessions  []aiSession
	hasEvents bool
	err       error
}

func newAISessionFeed() *aiSessionFeed {
	path, err := aiEventsPath()
	if err != nil {
		return nil
	}
	return &aiSessionFeed{tail: &aiEventTail{path: path}, activities: aiActivityReader{}}
}

func (feed *aiSessionFeed) read(now time.Time) aiSessionFeedMsg {
	if err := feed.tail.read(); err != nil {
		return aiSessionFeedMsg{err: err}
	}
	sessions := selectAISessions(foldAIHookEvents(feed.tail.events), now, false, false, aiProcessAlive, aiFileModified)
	feed.activities.annotate(sessions, now)
	return aiSessionFeedMsg{sessions: sessions, hasEvents: len(feed.tail.events) > 0}
}

func aiSessionCounts(sessions []aiSession) map[aiSessionState]int {
	counts := map[aiSessionState]int{}
	for _, session := range sessions {
		counts[session.State]++
	}
	return counts
}

// withSessionPane은 표 오른쪽에 세션 영역을 붙인다. 맨 아래 상태 줄은 화면 폭 전체를 쓰므로 붙이지 않는다.
func (model aiModel) withSessionPane(lines []string, width int) []string {
	paneWidth := width - aiBoxWidth - aiPaneGap
	if model.sessionFeed == nil || paneWidth < aiPaneMinWidth || len(lines) < 2 {
		return lines
	}
	body := len(lines) - 1
	pane := model.sessionPane(paneWidth, body)
	for index := range body {
		left := ansi.Truncate(lines[index], aiBoxWidth, "")
		right := ""
		if index < len(pane) {
			right = ansi.Truncate(pane[index], paneWidth, "")
		}
		lines[index] = padRight(left, aiBoxWidth) + richDim(" │ ") + right
	}
	return lines
}

func (model aiModel) sessionPane(width, height int) []string {
	counts := aiSessionCounts(model.sessions)
	head := []string{"sessions"}
	for _, state := range []aiSessionState{aiStateNeeds, aiStateFailed, aiStateWorking, aiStateWaiting} {
		if counts[state] > 0 {
			head = append(head, aiStateMarks[state]+" "+strconv.Itoa(counts[state]))
		}
	}
	title := richPaint(head[0], richMagenta, true) + richDim(" · "+strings.Join(head[1:], " · "))
	if len(head) == 1 {
		title = richPaint(head[0], richMagenta, true)
	}
	lines := []string{title}
	switch {
	case model.sessionsErr != nil:
		return append(lines, richPaint(model.sessionsErr.Error(), richRed, false))
	case !model.sessionsRead:
		return append(lines, richDim("reading the hook events"))
	case !model.sessionsHaveEvents:
		return append(lines, richDim("no hook events · run edc ai hooks install"))
	case len(model.sessions) == 0:
		return append(lines, richDim(T("observe.ai.sessions.none")))
	}
	for _, session := range model.sessions {
		if len(lines) == height {
			break
		}
		lines = append(lines, aiPaneLine(session, model.now, width))
	}
	return lines
}

// aiPaneTools는 좁은 영역에서 쓰는 도구 약어다. 설명에 폭을 더 준다.
var aiPaneTools = map[string]string{"claude": "cc", "codex": "cx"}

// aiPaneLine은 좁은 영역에 맞춘 한 줄이다. 상태는 기호로만 보이고 나머지 폭은 설명에 준다.
func aiPaneLine(session aiSession, now time.Time, width int) string {
	project := "-"
	if session.Cwd != "" {
		project = filepath.Base(session.Cwd)
	}
	head := strings.Join([]string{aiStateMarks[session.State], aiPaneTools[session.Tool], padRight(truncateName(project, 10), 10),
		padLeft(formatTopEventDuration(now.Sub(session.Since)), 6), padLeft(aiSessionTokens(session), 6)}, " ")
	detail := aiSessionDetail(session, now)
	if session.State == aiStateNeeds || session.State == aiStateFailed {
		detail = T("observe.ai.sessions.reason."+session.Reason) + " · " + detail
	}
	return aiSessionFit(head, detail, session, width, false)
}

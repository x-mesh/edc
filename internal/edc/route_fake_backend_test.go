package edc

import (
	"context"
	"fmt"
)

// fakeRouteBackend는 실제 호스트에서 캡처한 텍스트 fixture를 파서로 변환해 백엔드 값으로 돌려준다.
// 프로덕션은 netlink에서 읽지만, 테스트 입력은 지어낸 구조체가 아니라 실측 출력이라야 한다. 이번
// 프로젝트에서 발견된 route 결함은 전부 상상한 fixture가 실제 데이터와 달라서 생겼다.
type fakeRouteBackend struct {
	routeText    string
	ruleText     string
	neighborText string
	linkText     string
	// getResults는 주소별 ip route get 원문이다. 없는 주소는 오류를 돌려준다.
	getResults map[string]string
	// errs는 특정 호출을 실패시켜 오류 경로를 재현한다.
	errs map[string]error
	// routeSequence는 호출마다 다른 상태를 돌려준다. 변경 전후를 비교하는 경로를 재현할 때 쓴다.
	routeSequence []string
	writes        []fakeRouteWrite
}

func (backend *fakeRouteBackend) Routes(context.Context) ([]routeEntry, error) {
	if err := backend.errs["routes"]; err != nil {
		return nil, err
	}
	text := backend.routeText
	if len(backend.routeSequence) > 0 {
		text = backend.routeSequence[0]
		if len(backend.routeSequence) > 1 {
			backend.routeSequence = backend.routeSequence[1:]
		}
	}
	entries, _ := parseRouteTable(text)
	return entries, nil
}

func (backend *fakeRouteBackend) Rules(context.Context) ([]ipRule, error) {
	if err := backend.errs["rules"]; err != nil {
		return nil, err
	}
	rules, _ := parseIPRule(backend.ruleText)
	return rules, nil
}

func (backend *fakeRouteBackend) Neighbors(context.Context) ([]neighborEntry, error) {
	if err := backend.errs["neighbors"]; err != nil {
		return nil, err
	}
	return parseNeighbors(backend.neighborText), nil
}

func (backend *fakeRouteBackend) Links(context.Context) ([]linkInfo, error) {
	if err := backend.errs["links"]; err != nil {
		return nil, err
	}
	return parseLinks(backend.linkText), nil
}

func (backend *fakeRouteBackend) RouteTo(_ context.Context, address string) (routeGetResult, error) {
	text, ok := backend.getResults[address]
	if !ok {
		return routeGetResult{}, fmt.Errorf("no route to %s", address)
	}
	return parseRouteGet(text)
}

// writes는 백엔드가 받은 쓰기 조작을 순서대로 기록한다. 테스트는 이 목록으로 무엇이 실행됐는지 본다.
type fakeRouteWrite struct {
	Kind   string
	Entry  routeEntry
	NewVia string
}

func (backend *fakeRouteBackend) ReplaceRoute(_ context.Context, entry routeEntry, newVia string) error {
	backend.writes = append(backend.writes, fakeRouteWrite{Kind: "replace", Entry: entry, NewVia: newVia})
	return backend.errs["replace"]
}

func (backend *fakeRouteBackend) DeleteRoute(_ context.Context, entry routeEntry) error {
	backend.writes = append(backend.writes, fakeRouteWrite{Kind: "delete", Entry: entry})
	return backend.errs["delete"]
}

// wroteReplace는 기록된 쓰기 중 목적지와 새 via가 맞는 replace가 있는지 본다.
func (backend *fakeRouteBackend) wroteReplace(dest, newVia string) bool {
	for _, write := range backend.writes {
		if write.Kind == "replace" && write.Entry.Dest == dest && write.NewVia == newVia {
			return true
		}
	}
	return false
}

func (backend *fakeRouteBackend) wroteDelete(dest, via string) bool {
	for _, write := range backend.writes {
		if write.Kind == "delete" && write.Entry.Dest == dest && write.Entry.Via == via {
			return true
		}
	}
	return false
}

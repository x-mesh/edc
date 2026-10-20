package edc

import (
	"context"
	"os"
	"sync"
	"time"
)

// cleanNewestParallel은 가장 최근 파일을 동시에 찾는 후보의 수다. 후보마다 한 번 더 걷는 비용이 가장 느린 후보 하나에 머물게 한다.
const cleanNewestParallel = 4

type cleanNewestTask struct {
	item  int
	group int // 0보다 작으면 후보 전체다.
	paths []string
}

// cleanAddNewestWrite는 잰 후보와 묶음마다 가장 최근 파일의 수정 날짜를 채운다. 지워도 되는지 보는 근거이고, 읽지 못한 폴더가
// 있거나 상한에서 멈추면 부분 결과로 표시한다. 파일은 읽기만 한다.
//
// 폴더는 한 번만 걷는다. 묶음이 있는 후보는 묶음의 폴더를 걷고, 후보 전체의 날짜는 묶음 날짜의 최댓값과 맨 위 폴더에 놓인
// 파일에서 구한다. 후보 전체와 묶음을 따로 걸으면 같은 파일을 두 번 읽어 clean의 시간이 두 배가 된다.
func cleanAddNewestWrite(ctx context.Context, items []cleanItem) {
	var tasks []cleanNewestTask
	for index := range items {
		item := &items[index]
		if !item.counted() {
			continue
		}
		grouped := false
		for group := range item.Groups {
			if len(item.Groups[group].paths) > 0 {
				grouped = true
				tasks = append(tasks, cleanNewestTask{item: index, group: group, paths: item.Groups[group].paths})
			}
		}
		if !grouped && len(item.roots) > 0 {
			tasks = append(tasks, cleanNewestTask{item: index, group: -1, paths: item.roots})
		}
	}
	var running sync.WaitGroup
	slots := make(chan struct{}, cleanNewestParallel)
	for _, task := range tasks {
		running.Add(1)
		slots <- struct{}{}
		go func(task cleanNewestTask) {
			defer func() {
				<-slots
				running.Done()
			}()
			newest, partial := cleanNewestOf(ctx, task.paths)
			// 후보 전체와 묶음은 서로 다른 필드이고 묶음마다 다른 원소라 동시에 써도 겹치지 않는다.
			if task.group < 0 {
				items[task.item].NewestWrite, items[task.item].NewestPartial = newest, partial
				return
			}
			group := &items[task.item].Groups[task.group]
			group.NewestWrite, group.NewestPartial = newest, partial
		}(task)
	}
	running.Wait()
	for index := range items {
		item := &items[index]
		if !item.counted() || item.NewestWrite != nil || len(item.Groups) == 0 {
			continue
		}
		var newest *time.Time
		partial := false
		for _, group := range item.Groups {
			if group.NewestWrite != nil && (newest == nil || group.NewestWrite.After(*newest)) {
				newest = group.NewestWrite
			}
			partial = partial || group.NewestPartial
		}
		// 묶음이 하나의 상위 폴더를 나눈 후보는 그 폴더 바로 아래의 파일이 묶음에 들지 않는다.
		if len(item.roots) == 1 && !cleanRootIsGroup(item) {
			if loose := cleanNewestShallow(item.roots[0]); loose.After(timeOrZero(newest)) {
				utc := loose.UTC()
				newest = &utc
			}
		}
		item.NewestWrite, item.NewestPartial = newest, partial
	}
}

func timeOrZero(at *time.Time) time.Time {
	if at == nil {
		return time.Time{}
	}
	return *at
}

// cleanRootIsGroup는 묶음 폴더가 곧 후보가 잰 폴더인 후보인지 본다. 폴더마다 따로 잰 후보가 그렇다.
func cleanRootIsGroup(item *cleanItem) bool {
	if len(item.Groups) == 0 {
		return false
	}
	measured := map[string]bool{}
	for _, root := range item.roots {
		measured[root] = true
	}
	for _, group := range item.Groups {
		for _, path := range group.paths {
			if measured[path] {
				return true
			}
		}
	}
	return false
}

// cleanNewestOf는 폴더 여러 개의 가장 늦은 파일 날짜를 돌려준다.
func cleanNewestOf(ctx context.Context, paths []string) (*time.Time, bool) {
	var newest time.Time
	partial := false
	for _, path := range paths {
		found, cut := cleanNewestWrite(ctx, path)
		if found.After(newest) {
			newest = found
		}
		partial = partial || cut
	}
	if newest.IsZero() {
		return nil, partial
	}
	utc := newest.UTC()
	return &utc, partial
}

// cleanNewestShallow는 dir 바로 아래 일반 파일의 가장 늦은 수정 시각이다. 하위 폴더로 내려가지 않는다.
func cleanNewestShallow(dir string) time.Time {
	var newest time.Time
	entries, err := os.ReadDir(dir)
	if err != nil {
		return newest
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if info, err := entry.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// cleanNewestText는 가장 최근 파일 날짜 줄이다. 부분 결과이면 그 날짜보다 늦은 파일이 있을 수 있다.
func cleanNewestText(at *time.Time, partial bool) string {
	text := T("clean.newest", cleanDockerDate(at))
	if partial {
		return "≥ " + text
	}
	return text
}

func cleanGroupDate(group cleanGroup) string {
	if group.NewestWrite == nil {
		return ""
	}
	if group.NewestPartial {
		return "≥ " + cleanDockerDate(group.NewestWrite)
	}
	return cleanDockerDate(group.NewestWrite)
}

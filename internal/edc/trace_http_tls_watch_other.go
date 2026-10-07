//go:build !linux

package edc

// traceTLSExecProblem은 linux가 아니면 부르지 않는다. trace http는 linux에서만 돈다.
var traceTLSExecProblem = func() string { return "" }

package edc

// findSelfBind는 디렉터리를 자기 자신 위에 붙인 bind mount다. 그 파일은 다른 곳에 없으므로 건너뛰지 않는다.
func findSelfBind(mounts []diskMount, mount diskMount) bool { return duSelfBind(mounts, mount) }

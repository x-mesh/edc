package edc

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

const (
	// whereProbePort는 handshake만 하고 끊을 port다. 공개 endpoint가 모두 여기서 듣는다.
	whereProbePort = "443"
	// whereParallel은 동시에 여는 연결 수다. 너무 높이면 마지막 마일이 스스로 혼잡해져 값이 흐려진다.
	whereParallel = 8
	// whereDialTimeout은 handshake 하나의 상한이다. 공용 예산을 한 endpoint가 다 쓰지 못하게 막는다.
	whereDialTimeout = 4 * time.Second
	// cloudflareTraceURL은 anycast로 받은 PoP 코드를 돌려준다. 위치를 좁히는 데 쓴다.
	cloudflareTraceURL = "https://cloudflare.com/cdn-cgi/trace"
)

// regionEndpoint는 도시 하나에 대응하는 사업자별 접점이다.
// 도시를 단위로 삼아야 "어느 지역이 가까운가"라는 질문에 바로 답한다.
type regionEndpoint struct {
	city     string
	provider string
	region   string
	host     string
}

// regionEndpoints는 인증 없이 TCP handshake를 받아 주는 공개 endpoint다.
// Azure는 리전 이름이 붙은 공개 주소가 실제로 그 리전에서 끝나지 않아 넣지 않았다.
var regionEndpoints = []regionEndpoint{
	{"Seoul", "aws", "ap-northeast-2", "s3.ap-northeast-2.amazonaws.com"},
	{"Seoul", "gcp", "asia-northeast3", "storage.asia-northeast3.rep.googleapis.com"},
	{"Tokyo", "aws", "ap-northeast-1", "s3.ap-northeast-1.amazonaws.com"},
	{"Tokyo", "gcp", "asia-northeast1", "storage.asia-northeast1.rep.googleapis.com"},
	{"Hong Kong", "aws", "ap-east-1", "s3.ap-east-1.amazonaws.com"},
	{"Hong Kong", "gcp", "asia-east2", "storage.asia-east2.rep.googleapis.com"},
	{"Singapore", "aws", "ap-southeast-1", "s3.ap-southeast-1.amazonaws.com"},
	{"Singapore", "gcp", "asia-southeast1", "storage.asia-southeast1.rep.googleapis.com"},
	{"Mumbai", "aws", "ap-south-1", "s3.ap-south-1.amazonaws.com"},
	{"Mumbai", "gcp", "asia-south1", "storage.asia-south1.rep.googleapis.com"},
	{"Sydney", "aws", "ap-southeast-2", "s3.ap-southeast-2.amazonaws.com"},
	{"Sydney", "gcp", "australia-southeast1", "storage.australia-southeast1.rep.googleapis.com"},
	{"Virginia", "aws", "us-east-1", "s3.us-east-1.amazonaws.com"},
	{"Virginia", "gcp", "us-east4", "storage.us-east4.rep.googleapis.com"},
	{"Oregon", "aws", "us-west-2", "s3.us-west-2.amazonaws.com"},
	{"Oregon", "gcp", "us-west1", "storage.us-west1.rep.googleapis.com"},
	{"Iowa", "gcp", "us-central1", "storage.us-central1.rep.googleapis.com"},
	{"Frankfurt", "aws", "eu-central-1", "s3.eu-central-1.amazonaws.com"},
	{"Frankfurt", "gcp", "europe-west3", "storage.europe-west3.rep.googleapis.com"},
	{"Ireland", "aws", "eu-west-1", "s3.eu-west-1.amazonaws.com"},
	{"Belgium", "gcp", "europe-west1", "storage.europe-west1.rep.googleapis.com"},
	{"Sao Paulo", "aws", "sa-east-1", "s3.sa-east-1.amazonaws.com"},
	{"Sao Paulo", "gcp", "southamerica-east1", "storage.southamerica-east1.rep.googleapis.com"},
}

// whereMeasurement은 endpoint 하나의 측정 결과다.
type whereMeasurement struct {
	City     string        `json:"city"`
	Provider string        `json:"provider"`
	Region   string        `json:"region"`
	Host     string        `json:"host"`
	Min      time.Duration `json:"-"`
	Median   time.Duration `json:"-"`
	Jitter   time.Duration `json:"-"`
	MinMS    float64       `json:"min_ms"`
	MedianMS float64       `json:"median_ms"`
	JitterMS float64       `json:"jitter_ms"`
	Samples  int           `json:"samples"`
	Error    string        `json:"error,omitempty"`
}

// whereLocation은 이 host가 붙어 있는 망의 모습이다.
type whereLocation struct {
	PublicIP        string `json:"public_ip,omitempty"`
	Country         string `json:"country,omitempty"`
	Region          string `json:"region,omitempty"`
	City            string `json:"city,omitempty"`
	Org             string `json:"org,omitempty"`
	CloudflarePoP   string `json:"cloudflare_pop,omitempty"`
	CloudflareLoc   string `json:"cloudflare_loc,omitempty"`
	Interface       string `json:"interface,omitempty"`
	LocalAddress    string `json:"local_address,omitempty"`
	Gateway         string `json:"gateway,omitempty"`
	BehindNAT       bool   `json:"behind_nat"`
	CarrierGradeNAT bool   `json:"carrier_grade_nat"`
	Tunnel          bool   `json:"tunnel"`
}

type whereReport struct {
	Location whereLocation      `json:"location"`
	Regions  []whereMeasurement `json:"regions"`
}

func runWhere(args []string, version string) int {
	options := configuredCommon(20 * time.Second)
	set := flag.NewFlagSet("where", flag.ContinueOnError)
	set.SetOutput(os.Stderr)
	bindCommon(set, &options)
	provider := set.String("provider", configuredString(activeConfig.Defaults.Where.Provider, "all"), T("command.where.option.provider"))
	count := set.Int("count", configuredInt(activeConfig.Defaults.Where.Count, 3), T("command.where.option.count"))
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, T("observe.where.usage"))
		return 2
	}
	if *count < 1 {
		fmt.Fprintln(os.Stderr, T("observe.where.count_minimum"))
		return 2
	}
	endpoints, err := selectRegionEndpoints(*provider)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), options.timeout)
	defer cancel()

	progress := startWhereProgress(len(endpoints), cancel)
	var (
		group        sync.WaitGroup
		location     whereLocation
		measurements []whereMeasurement
	)
	group.Add(2)
	go func() { defer group.Done(); location = collectWhereLocation(ctx) }()
	go func() {
		defer group.Done()
		measurements = measureRegions(ctx, endpoints, *count, progress.advance)
	}()
	group.Wait()
	progress.finish()

	report := whereReport{Location: location, Regions: measurements}

	// 화면에서는 자기 주소를 그대로 보여 준다. 이 명령은 그 값을 보려고 실행한다.
	// 공유되는 산출물인 JSON에만 --redact를 적용해 edc info와 규칙을 맞춘다.
	if options.jsonPath != "" {
		if options.redact {
			report.Location = redactWhereLocation(report.Location)
		}
		if err := writeJSONOutput(options.jsonPath, report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		return whereExitCode(measurements)
	}
	printWhere(os.Stdout, report, options.verbose)
	return whereExitCode(measurements)
}

func redactWhereLocation(location whereLocation) whereLocation {
	location.PublicIP = redactIPAddresses(location.PublicIP)
	location.LocalAddress = redactIPAddresses(location.LocalAddress)
	location.Gateway = redactIPAddresses(location.Gateway)
	return location
}

// whereExitCode는 하나도 재지 못했을 때만 실패로 본다. 일부 지역이 막혀 있는 것은 흔한 일이다.
func whereExitCode(measurements []whereMeasurement) int {
	for _, measurement := range measurements {
		if measurement.Error == "" {
			return 0
		}
	}
	return 1
}

func selectRegionEndpoints(provider string) ([]regionEndpoint, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "all" {
		return regionEndpoints, nil
	}
	var selected []regionEndpoint
	for _, endpoint := range regionEndpoints {
		if endpoint.provider == provider {
			selected = append(selected, endpoint)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("%s", T("observe.where.unknown_provider", provider))
	}
	return selected, nil
}

// measureRegions는 endpoint마다 TCP handshake 시간을 여러 번 잰다.
// 이름은 먼저 한 번만 풀고 그 주소로만 연결해, DNS 시간이 거리 값에 섞이지 않게 한다.
func measureRegions(ctx context.Context, endpoints []regionEndpoint, count int, advance func(int)) []whereMeasurement {
	results := make([]whereMeasurement, len(endpoints))
	tokens := make(chan struct{}, whereParallel)
	var (
		group     sync.WaitGroup
		completed atomic.Int64
	)

	for index, endpoint := range endpoints {
		group.Add(1)
		go func(index int, endpoint regionEndpoint) {
			defer group.Done()
			tokens <- struct{}{}
			defer func() { <-tokens }()
			results[index] = measureEndpoint(ctx, endpoint, count)
			if advance != nil {
				advance(int(completed.Add(1)))
			}
		}(index, endpoint)
	}
	group.Wait()

	sort.SliceStable(results, func(i, j int) bool {
		if (results[i].Error == "") != (results[j].Error == "") {
			return results[i].Error == ""
		}
		return results[i].Min < results[j].Min
	})
	return results
}

func measureEndpoint(ctx context.Context, endpoint regionEndpoint, count int) whereMeasurement {
	measurement := whereMeasurement{
		City: endpoint.city, Provider: endpoint.provider,
		Region: endpoint.region, Host: endpoint.host,
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, endpoint.host)
	if err != nil || len(addresses) == 0 {
		measurement.Error = T("observe.where.error.resolve")
		return measurement
	}

	// 첫 주소만 쓰면 IPv6이 앞에 오는 이름에서 IPv4만 되는 host가 전부 unreachable로 보인다.
	// 닿는 주소를 하나 찾을 때까지 넘어가고, 찾은 뒤에는 그 주소로만 반복해 값이 섞이지 않게 한다.
	target, err := firstReachableAddress(ctx, addresses)
	if err != nil {
		if isCancelled(ctx) {
			measurement.Error = T("observe.where.error.timeout")
		} else {
			measurement.Error = T("observe.where.error.connect")
		}
		return measurement
	}

	samples := []time.Duration{}
	for attempt := 0; attempt < count; attempt++ {
		elapsed, err := dialOnce(ctx, target)
		if err != nil {
			continue
		}
		samples = append(samples, elapsed)
	}
	if len(samples) == 0 {
		// 여기까지 왔다면 첫 연결은 됐다는 뜻이므로, 남은 원인은 예산 소진뿐이다.
		measurement.Error = T("observe.where.error.timeout")
		return measurement
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	measurement.Samples = len(samples)
	measurement.Min = samples[0]
	measurement.Median = samples[len(samples)/2]
	measurement.Jitter = samples[len(samples)-1] - samples[0]
	measurement.MinMS = milliseconds(measurement.Min)
	measurement.MedianMS = milliseconds(measurement.Median)
	measurement.JitterMS = milliseconds(measurement.Jitter)
	return measurement
}

// firstReachableAddress는 닿는 주소를 하나 고른다. 그 주소로 잰 첫 값은 버린다.
func firstReachableAddress(ctx context.Context, addresses []net.IPAddr) (string, error) {
	var lastErr error
	for _, address := range addresses {
		if isCancelled(ctx) {
			return "", ctx.Err()
		}
		target := net.JoinHostPort(address.IP.String(), whereProbePort)
		if _, err := dialOnce(ctx, target); err != nil {
			lastErr = err
			continue
		}
		return target, nil
	}
	if lastErr == nil {
		lastErr = errors.New(T("observe.where.error.connect"))
	}
	return "", lastErr
}

func isCancelled(ctx context.Context) bool { return ctx.Err() != nil }

// dialOnce는 handshake 하나에만 시간을 준다.
// 개별 상한이 없으면 SYN이 버려지는 endpoint 하나가 전체 예산을 다 쓰고,
// 실제로 닿는 지역까지 unreachable로 보고된다.
func dialOnce(ctx context.Context, target string) (time.Duration, error) {
	attempt, cancel := context.WithTimeout(ctx, whereDialTimeout)
	defer cancel()
	started := time.Now()
	conn, err := (&net.Dialer{}).DialContext(attempt, "tcp", target)
	if err != nil {
		return 0, err
	}
	elapsed := time.Since(started)
	conn.Close()
	return elapsed, nil
}

func milliseconds(value time.Duration) float64 {
	return float64(value.Microseconds()) / 1000
}

func collectWhereLocation(ctx context.Context) whereLocation {
	var location whereLocation
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		if info, err := fetchPublicNetworkInfo(ctx); err == nil {
			location.PublicIP, location.Country = info.IP, info.Country
			location.Region, location.City, location.Org = info.Region, info.City, info.Org
		}
	}()
	go func() {
		defer group.Done()
		location.CloudflarePoP, location.CloudflareLoc = fetchCloudflarePoP(ctx)
	}()
	group.Wait()

	iface, gateway := collectDefaultRoute()
	location.Interface, location.Gateway = iface, gateway
	location.Tunnel = isTunnelInterface(iface)
	if interfaces, err := networkInterfaces(iface, gateway); err == nil {
		for _, candidate := range interfaces {
			if candidate.Name == iface {
				location.LocalAddress = candidate.Address
				break
			}
		}
	}
	location.CarrierGradeNAT = isCarrierGradeNAT(location.PublicIP)
	location.BehindNAT = isPrivateAddress(location.LocalAddress) && location.PublicIP != "" && location.LocalAddress != location.PublicIP
	return location
}

// fetchCloudflarePoP는 anycast가 어느 PoP으로 보냈는지 읽는다. 도시 단위로 위치를 좁혀 준다.
func fetchCloudflarePoP(ctx context.Context) (string, string) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, cloudflareTraceURL, nil)
	if err != nil {
		return "", ""
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", ""
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	if err != nil {
		return "", ""
	}
	return parseCloudflareTrace(string(body))
}

func parseCloudflareTrace(body string) (string, string) {
	var pop, loc string
	for _, line := range strings.Split(body, "\n") {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "colo":
			pop = strings.TrimSpace(value)
		case "loc":
			loc = strings.TrimSpace(value)
		}
	}
	return pop, loc
}

func isTunnelInterface(name string) bool {
	for _, prefix := range []string{"utun", "tun", "tap", "ppp", "wg", "ipsec"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func isPrivateAddress(value string) bool {
	address := net.ParseIP(value)
	if address == nil {
		return false
	}
	return address.IsPrivate() || address.IsLinkLocalUnicast()
}

// isCarrierGradeNAT은 RFC 6598의 100.64.0.0/10을 본다. 이동통신망과 일부 ISP가 이 대역을 쓴다.
func isCarrierGradeNAT(value string) bool {
	address := net.ParseIP(value)
	if address == nil {
		return false
	}
	_, block, err := net.ParseCIDR("100.64.0.0/10")
	if err != nil {
		return false
	}
	return block.Contains(address)
}

// padRight와 padLeft는 한글이 두 칸을 차지하는 것을 반영해 열을 맞춘다.
// fmt의 %-*s는 rune 수만 세므로 한글 라벨이 섞이면 열이 어긋난다.
func padRight(value string, width int) string {
	if gap := width - liveWidth(value); gap > 0 {
		return value + strings.Repeat(" ", gap)
	}
	return value
}

func padLeft(value string, width int) string {
	if gap := width - liveWidth(value); gap > 0 {
		return strings.Repeat(" ", gap) + value
	}
	return value
}

// cityRow는 도시 한 곳의 결과다. 여러 사업자를 재면 가장 빠른 쪽을 그 도시의 값으로 삼는다.
type cityRow struct {
	city    string
	best    whereMeasurement
	all     []whereMeasurement
	reached bool
}

func groupByCity(measurements []whereMeasurement) []cityRow {
	order := make([]string, 0, len(measurements))
	rows := map[string]*cityRow{}
	for _, measurement := range measurements {
		row, seen := rows[measurement.City]
		if !seen {
			row = &cityRow{city: measurement.City}
			rows[measurement.City] = row
			order = append(order, measurement.City)
		}
		row.all = append(row.all, measurement)
		if measurement.Error != "" {
			continue
		}
		if !row.reached || measurement.Min < row.best.Min {
			row.best, row.reached = measurement, true
		}
	}
	result := make([]cityRow, 0, len(order))
	for _, city := range order {
		result = append(result, *rows[city])
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].reached != result[j].reached {
			return result[i].reached
		}
		return result[i].best.Min < result[j].best.Min
	})
	return result
}

func printWhere(writer io.Writer, report whereReport, verbose bool) {
	printWhereLocation(writer, report.Location)

	rows := groupByCity(report.Regions)
	fmt.Fprintf(writer, "\n%s\n", T("observe.where.heading.nearest"))
	if verbose {
		printWhereDetail(writer, rows)
	} else {
		printWhereSummary(writer, rows)
	}
	if line := whereConclusion(rows, report.Location); line != "" {
		fmt.Fprintf(writer, "\n%s\n", line)
	}
}

// whereLabelWidth는 라벨 열의 폭이다. 번역마다 라벨 길이가 달라 실행 시점에 잰다.
// 가장 긴 라벨 뒤에 두 칸을 남겨 값이 라벨에 붙지 않게 한다.
func whereLabelWidth() int {
	width := 0
	for _, label := range []string{"public IP", "Cloudflare", T("observe.where.label.route"), T("observe.where.label.network")} {
		if value := liveWidth(label); value > width {
			width = value
		}
	}
	return width + 2
}

func printWhereLocation(writer io.Writer, location whereLocation) {
	fmt.Fprintln(writer, T("observe.where.heading.location"))
	width := whereLabelWidth()
	line := func(label, value string) {
		if value == "" {
			return
		}
		fmt.Fprintf(writer, "  %s %s\n", padRight(label, width), value)
	}
	line("public IP", joinFields(location.PublicIP, whereGeography(location), location.Org))
	if location.CloudflarePoP != "" {
		line("Cloudflare", joinFields(location.CloudflarePoP+" PoP", location.CloudflareLoc))
	}
	if location.Interface != "" {
		route := location.Interface
		if location.LocalAddress != "" {
			route += "  " + location.LocalAddress
		}
		if location.Gateway != "" {
			route += " → " + location.Gateway
		}
		line(T("observe.where.label.route"), route)
	}
	line(T("observe.where.label.network"), whereNetworkShape(location))
}

// whereGeography는 나라, 광역, 도시를 잇는다. ipinfo가 광역과 도시에 같은 이름을 주는 곳이 있어 중복은 뺀다.
func whereGeography(location whereLocation) string {
	if strings.EqualFold(location.Region, location.City) {
		return joinFields(location.Country, location.City)
	}
	return joinFields(location.Country, location.Region, location.City)
}

// whereNetworkShape은 확인한 것만 적는다. 회선 종류처럼 추측이 필요한 값은 넣지 않는다.
func whereNetworkShape(location whereLocation) string {
	var parts []string
	switch {
	case location.CarrierGradeNAT:
		parts = append(parts, T("observe.where.shape.carrier_grade_nat"))
	case location.BehindNAT:
		parts = append(parts, T("observe.where.shape.nat"))
	case location.PublicIP != "" && location.LocalAddress == location.PublicIP:
		parts = append(parts, T("observe.where.shape.public_direct"))
	}
	if location.Tunnel {
		parts = append(parts, T("observe.where.shape.tunnel"))
	}
	return strings.Join(parts, "  ·  ")
}

func joinFields(values ...string) string {
	var kept []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			kept = append(kept, value)
		}
	}
	return strings.Join(kept, "  ·  ")
}

const (
	// whereValueMinWidth는 "1234.5ms" 같은 값이 들어갈 최소 폭이다.
	whereValueMinWidth = 9
	// whereProviderMinWidth는 aws, gcp 같은 사업자 이름이 들어갈 최소 폭이다.
	whereProviderMinWidth = 8
)

// whereValueWidth는 숫자 열의 폭이다. 번역한 머리글이 최소 폭보다 넓으면 그만큼 넓힌다.
func whereValueWidth() int {
	width := whereValueMinWidth
	for _, label := range []string{
		T("observe.where.column.min"), T("observe.where.column.median"),
		T("observe.where.column.jitter"), T("observe.where.unreachable"),
	} {
		if value := liveWidth(label); value > width {
			width = value
		}
	}
	return width
}

func whereProviderWidth() int {
	width := whereProviderMinWidth
	if value := liveWidth(T("observe.where.column.provider")); value > width {
		width = value
	}
	return width
}

func printWhereSummary(writer io.Writer, rows []cityRow) {
	width := cityColumnWidth(rows)
	value := whereValueWidth()
	fmt.Fprintf(writer, "  %s  %s  %s  %s  %s\n", padRight("", width),
		padLeft(T("observe.where.column.min"), value), padLeft(T("observe.where.column.median"), value),
		padLeft(T("observe.where.column.jitter"), value), T("observe.where.column.provider"))
	for _, row := range rows {
		if !row.reached {
			fmt.Fprintf(writer, "  %s  %s\n", padRight(row.city, width), padLeft(T("observe.where.unreachable"), value))
			continue
		}
		fmt.Fprintf(writer, "  %s  %s  %s  %s  %s\n", padRight(row.city, width),
			padLeft(formatMillis(row.best.Min), value),
			padLeft(formatMillis(row.best.Median), value),
			padLeft(formatMillis(row.best.Jitter), value), row.best.Provider)
	}
}

func printWhereDetail(writer io.Writer, rows []cityRow) {
	width := cityColumnWidth(rows)
	value := whereValueWidth()
	provider := whereProviderWidth()
	fmt.Fprintf(writer, "  %s  %s  %s  %s  %s  %s\n", padRight("", width), padRight(T("observe.where.column.provider"), provider),
		padLeft(T("observe.where.column.min"), value), padLeft(T("observe.where.column.median"), value),
		padLeft(T("observe.where.column.jitter"), value), "region")
	for _, row := range rows {
		for _, measurement := range row.all {
			if measurement.Error != "" {
				fmt.Fprintf(writer, "  %s  %s  %s  %s\n", padRight(row.city, width),
					padRight(measurement.Provider, provider), padLeft("–", value), measurement.Error)
				continue
			}
			fmt.Fprintf(writer, "  %s  %s  %s  %s  %s  %s\n", padRight(row.city, width),
				padRight(measurement.Provider, provider),
				padLeft(formatMillis(measurement.Min), value),
				padLeft(formatMillis(measurement.Median), value),
				padLeft(formatMillis(measurement.Jitter), value), measurement.Region)
		}
	}
}

func cityColumnWidth(rows []cityRow) int {
	width := 0
	for _, row := range rows {
		if len(row.city) > width {
			width = len(row.city)
		}
	}
	return width
}

func formatMillis(value time.Duration) string {
	return fmt.Sprintf("%.1fms", milliseconds(value))
}

// whereConclusion은 가장 가까운 지역과 그다음의 차이를 한 줄로 남긴다.
// 차이가 작으면 순위가 실행마다 뒤집히므로 그 사실을 알린다.
func whereConclusion(rows []cityRow, location whereLocation) string {
	var reached []cityRow
	for _, row := range rows {
		if row.reached {
			reached = append(reached, row)
		}
	}
	if len(reached) == 0 {
		return T("observe.where.conclusion.none")
	}
	nearest := reached[0]
	if len(reached) == 1 {
		return T("observe.where.conclusion.only", nearest.city)
	}
	gap := reached[1].best.Min - nearest.best.Min
	if gap < 5*time.Millisecond {
		return T("observe.where.conclusion.close", nearest.city, reached[1].city, formatMillis(gap))
	}
	return T("observe.where.conclusion.clear", nearest.city, reached[1].city, formatMillis(gap))
}

// whereProgressModel은 측정이 도는 동안 한 줄로 진행을 보여 준다.
// where는 지역 수만큼 handshake를 열어 1초 넘게 걸리므로, 멈춘 것처럼 보이지 않게 한다.
type whereProgressModel struct {
	spinner spinner.Model
	done    int
	total   int
	started time.Time
	now     func() time.Time
	cancel  context.CancelFunc
	stopped bool
}

type whereProgressMsg struct{ done int }

type whereFinishedMsg struct{}

func newWhereProgressModel(total int, cancel context.CancelFunc) whereProgressModel {
	return whereProgressModel{
		spinner: liveSpinner(), total: total, started: time.Now(),
		now: time.Now, cancel: cancel,
	}
}

func (model whereProgressModel) Init() tea.Cmd { return model.spinner.Tick }

func (model whereProgressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case whereProgressMsg:
		model.done = value.done
		return model, nil
	case whereFinishedMsg:
		model.stopped = true
		return model, tea.Quit
	case tea.KeyPressMsg:
		if key := value.String(); key == "ctrl+c" || key == "q" || key == "esc" {
			if model.cancel != nil {
				model.cancel()
			}
			model.stopped = true
			return model, tea.Quit
		}
	}
	var command tea.Cmd
	model.spinner, command = model.spinner.Update(msg)
	return model, command
}

func (model whereProgressModel) View() tea.View {
	if model.stopped {
		// 진행 줄은 결과를 가리지 않도록 화면에서 지운다.
		return liveFrame("", 1)
	}
	elapsed := model.now().Sub(model.started).Seconds()
	line := fmt.Sprintf("%s  %s  %d/%d  ·  %.1fs", model.spinner.View(), T("observe.where.progress"), model.done, model.total, elapsed)
	return liveFrame(line, 1)
}

// whereProgress는 진행 화면을 감싼다. terminal이 아니면 아무것도 하지 않는다.
type whereProgress struct{ live *liveProgram }

func startWhereProgress(total int, cancel context.CancelFunc) *whereProgress {
	if !liveTerminal() {
		return &whereProgress{}
	}
	live, err := startLiveProgram(newWhereProgressModel(total, cancel), func() {})
	if err != nil {
		return &whereProgress{}
	}
	return &whereProgress{live: live}
}

func (progress *whereProgress) advance(done int) {
	if progress.live != nil {
		progress.live.send(whereProgressMsg{done: done})
	}
}

func (progress *whereProgress) finish() {
	if progress.live != nil {
		_, _ = progress.live.finish(whereFinishedMsg{})
	}
}

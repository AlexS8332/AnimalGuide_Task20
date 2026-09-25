//go:build edge

// Проверки интерфейса в настоящем браузере: headless Microsoft Edge
// открывает встроенный фронтенд, сценарий testdata/edge/scenario.js
// нажимает те же кнопки, что и человек, и пишет итог в <pre id="test-log">,
// а тест снимает DOM (--dump-dom) и разбирает строки OK/FAIL. Модель и
// источники подставные — сети и ключа не нужно.
//
// Запуск: go test -tags edge -run TestEdge -v .
// Снимки экрана: EDGE_SHOTS=каталог go test -tags edge -run TestEdge .
// Путь к браузеру, если он не в стандартном месте: EDGE_PATH.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexS8332/AnimalGuide_Task20/internal/agent"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/agents"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/agents/agentstest"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/collection"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/compiler"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/extract"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/features"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/history"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/llm"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/llm/llmtest"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/memory"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/persona"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/profile"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/runs"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/server"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/store"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/tools"
	"github.com/AlexS8332/AnimalGuide_Task20/internal/tools/toolstest"
)

// findEdge — путь к msedge: EDGE_PATH или стандартные места установки.
func findEdge() string {
	if p := os.Getenv("EDGE_PATH"); p != "" {
		return p
	}
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
			if dir := os.Getenv(env); dir != "" {
				candidates = append(candidates, filepath.Join(dir, "Microsoft", "Edge", "Application", "msedge.exe"))
			}
		}
	case "darwin":
		candidates = append(candidates, "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge")
	default:
		candidates = append(candidates, "microsoft-edge", "microsoft-edge-stable")
	}
	for _, c := range candidates {
		if filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		} else if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return ""
}

// Реплика с предпочтениями: извлекатель раскладывает её по профилю, памяти
// и карточке фактов — под ответом появляются чипы.
const edgePrefs = `{"profile":{"set":[{"field":"length","value":"short","scope":"always","quote":"пиши мне всегда коротко"}]},
 "memory":{"set":[{"layer":"long","key":"интерес","value":"хищники тайги"}]},
 "facts":{"set":[{"key":"тема","value":"чем питаются рыси"}]}}`

// edgeCompiler — добросовестный составитель подборки: план, затем по
// утверждению — первый вид.
func edgeCompiler(req llm.Request, step int) llm.Response {
	user := agentstest.LastUser(req)
	switch {
	case strings.Contains(user, "Собери подборку"):
		if step == 0 {
			return llmtest.ToolCall("plan", `{"goal":"доклад о кошках","species":["рысь","манул"]}`)
		}
		return llmtest.Text("План: рысь, манул. Утверждаете?")
	case strings.Contains(user, "утверждаю"):
		steps := []llm.Response{
			llmtest.ToolCall("approve", `{"quote":"план утверждаю"}`),
			llmtest.ToolCall("deliver", `{"n":1}`),
			llmtest.ToolCall("step_done", `{"n":1,"result":"карточка собрана"}`),
		}
		if step < len(steps) {
			return steps[step]
		}
		return llmtest.Text("Первый вид собран.")
	}
	return llmtest.Text("…")
}

// edgeApp — приложение, собранное как в main, но на подставной модели и
// подставных источниках, с данными во временном каталоге.
type edgeApp struct {
	m       *runs.Manager
	handler http.Handler
	// Диалоги сценария: основной (карточка, ветки, сравнение), подборка и
	// пустой.
	main, coll, empty string
	// facts — подставной демон «Интересных фактов» за /api/facts/.
	facts *edgeFacts
	// pipe — подставной REST конвейера за /api/pipeline/.
	pipe *edgePipe
}

func newEdgeApp(t *testing.T) *edgeApp {
	t.Helper()
	wiki, gbif := toolstest.NewWiki(), toolstest.NewGBIF()
	t.Cleanup(wiki.Close)
	t.Cleanup(gbif.Close)
	brain := &agentstest.Brain{Compiler: edgeCompiler}
	brain.Extract = func(req llm.Request) (string, error) {
		if strings.Contains(agentstest.LastUser(req), "коротко") {
			return edgePrefs, nil
		}
		return `{"profile":{"set":[]},"memory":{"set":[]},"facts":{"set":[]}}`, nil
	}
	brain.LeadScript = func(llm.Request, int) llm.Response {
		return llmtest.Text("Рысь охотится на зайцев, реже — на косуль и птиц.")
	}
	fake := &llmtest.Fake{Fn: brain.Chat}
	registry := features.Catalog()
	data := store.NewDir(t.TempDir())
	local := tools.MustRegistry(tools.LocalTools(tools.NewFetcher(), wiki.URL, gbif.URL)...)
	deps := agents.Deps{Runner: agent.Runner{LLM: fake, Model: llm.DefaultModel}, Features: registry, Sources: agents.Local{Registry: local}}
	people := &persona.Hook{Memory: memory.NewStore(data), Profiles: profile.NewStore(data),
		Extractor: extract.Extractor{LLM: fake, Model: llm.DefaultModel}}
	compile := &compiler.Hook{Agents: deps, Store: collection.NewStore(data)}
	m := runs.NewManager(runs.Config{
		Agents: deps, Store: history.NewStore(data), Registry: registry, Defaults: registry.Defaults(),
		Timeout: time.Minute, Window: history.DefaultWindow, KeepToolRunes: history.DefaultKeepToolRunes,
		Hooks: []runs.Hook{compile, people},
	})
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{"model": llm.DefaultModel, "window": history.DefaultWindow, "contextLimit": defaultContextLimit}
	for k, v := range persona.Meta() {
		meta[k] = v
	}
	a := &edgeApp{m: m, facts: newEdgeFacts(), pipe: newEdgePipe()}
	a.handler = server.New(m, static, meta, append(people.Extension(), compile.Extension(m)...)...)
	a.seed(t)
	return a
}

// turn — ход и ожидание его конца; conv пусто — новый диалог.
func (a *edgeApp) turn(t *testing.T, conv string, req agents.Request) runs.Detail {
	t.Helper()
	var s *runs.Session
	var err error
	if conv == "" {
		s, err = a.m.Start(runs.StartOptions{Request: req})
	} else {
		s, err = a.m.Send(conv, req)
	}
	if err != nil {
		t.Fatal(err)
	}
	v := s.Wait(30 * time.Second)
	if v.Status != runs.StatusDone {
		t.Fatalf("ход %+v: %s %s", req, v.Status, v.Error)
	}
	d, _ := a.m.Get(v.ConversationID)
	return d
}

// seed заводит диалоги, по которым ходит сценарий. Порядок важен: список
// диалогов отсортирован по времени, пустой — последним.
func (a *edgeApp) seed(t *testing.T) {
	t.Helper()
	d := a.turn(t, "", agents.Request{Text: "Собери подборку: кошки нашей фауны, два вида"})
	a.coll = d.ID
	a.turn(t, a.coll, agents.Request{Text: "План утверждаю"})

	d = a.turn(t, "", agents.Request{Text: "рысь"})
	a.main = d.ID
	cardID := d.Cards.Cards[0].ID
	a.turn(t, a.main, agents.Request{Kind: "section", CardID: cardID, Topic: "diet"})
	a.turn(t, a.main, agents.Request{Kind: "node", CardID: cardID, NodeKey: toolstest.KeyFelidae, NodeName: "Кошачьи"})
	a.turn(t, a.main, agents.Request{Text: "Пиши мне всегда коротко. Мне интересны хищники тайги: чем питается рысь?"})
	// Сравнение само ставит точку «до сравнения» и уходит в свою ветку.
	a.turn(t, a.main, agents.Request{Kind: "compare", A: "рысь", B: "манул", Text: "Сравни: рысь и манул"})

	e, err := a.m.Create(runs.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a.empty = e.ID
}

// page — сервер для браузера: настоящий обработчик приложения, но в
// index.html после app.js подключён сценарий, а /api/facts/ отвечает
// подставной демон (edgeFacts); для снимков — ещё и стиль, при
// котором прокручивается body, а не окно).
func (a *edgeApp) page(t *testing.T, shots bool) *httptest.Server {
	t.Helper()
	index, err := fs.ReadFile(webFiles, "web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	scenario, err := os.ReadFile(filepath.Join("testdata", "edge", "scenario.js"))
	if err != nil {
		t.Fatal(err)
	}
	inject := `<script src="/edge-scenario.js"></script>`
	if shots {
		inject = `<style>html{height:100%;overflow:hidden}body{height:100%;overflow:auto}</style>` + inject
	}
	patched := strings.Replace(string(index), "</body>", inject+"\n</body>", 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/edge-scenario.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Write(scenario)
	})
	mux.Handle("/api/facts/", a.facts)
	mux.Handle("/api/pipeline/", a.pipe)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(patched))
			return
		}
		a.handler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// edgeRun запускает headless Edge; out — --dump-dom или --screenshot=….
func edgeRun(t *testing.T, edge, url string, size string, out ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	args := append([]string{"--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--user-data-dir=" + t.TempDir(), "--virtual-time-budget=30000",
		"--window-size=" + size}, out...)
	cmd := exec.CommandContext(ctx, edge, append(args, url)...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("edge: %v\n%s", err, stderr.String())
	}
	return stdout.String()
}

var logRe = regexp.MustCompile(`(?s)<pre id="test-log"[^>]*>(.*?)</pre>`)

// TestEdge — проверки интерфейса по сценариям. Каждая строка OK/FAIL
// сценария — отдельный подтест.
func TestEdge(t *testing.T) {
	edge := findEdge()
	if edge == "" {
		t.Skip("Microsoft Edge не найден (задайте EDGE_PATH)")
	}
	a := newEdgeApp(t)
	srv := a.page(t, false)
	scenarios := []struct{ name, conv string }{
		{"main", a.main},
		{"collection", a.coll},
		{"empty", a.empty},
		{"facts", a.main},
		{"facts-down", a.main},
		{"pipeline", a.main},
	}
	total := 0
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			dom := edgeRun(t, edge, fmt.Sprintf("%s/?scenario=%s#c=%s", srv.URL, sc.name, sc.conv), "1400,900", "--dump-dom")
			m := logRe.FindStringSubmatch(dom)
			if m == nil {
				t.Fatalf("сценарий не оставил журнала; DOM начинается так:\n%.2000s", dom)
			}
			lines := strings.Split(strings.TrimSpace(html.UnescapeString(m[1])), "\n")
			done := false
			for _, line := range lines {
				status, rest, _ := strings.Cut(line, " ")
				name, reason, _ := strings.Cut(rest, " — ")
				switch status {
				case "OK", "FAIL":
					total++
					t.Run(name, func(t *testing.T) {
						if status == "FAIL" {
							t.Error(reason)
						}
					})
				case "DONE":
					done = true
				default:
					t.Log(line)
				}
			}
			if !done {
				t.Errorf("сценарий не дошёл до конца:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
	if total < 20 {
		t.Errorf("проверок %d, а должно быть не меньше 20", total)
	}

	if dir := os.Getenv("EDGE_SHOTS"); dir != "" {
		// Снимки: body прокручивается сам, иначе headless Edge рисует
		// прокрученную страницу со сдвигом (см. README).
		shot := a.page(t, true)
		abs, _ := filepath.Abs(dir)
		os.MkdirAll(abs, 0o755)
		for _, s := range []struct{ file, scenario, conv string }{
			{"main-top.png", "shot-top", a.main},
			{"main-bottom.png", "shot-bottom", a.main},
			{"collection.png", "shot-bottom", a.coll},
			{"empty.png", "shot-top", a.empty},
			{"window-people.png", "shot-people", a.main},
			{"facts-feed.png", "shot-facts", a.main},
			{"facts-detail.png", "shot-facts-detail", a.main},
			{"facts-summary.png", "shot-facts-summary", a.main},
			{"facts-down.png", "shot-facts-down", a.main},
			{"pipeline.png", "shot-pipeline", a.main},
		} {
			edgeRun(t, edge, fmt.Sprintf("%s/?scenario=%s#c=%s", shot.URL, s.scenario, s.conv), "1400,900",
				"--screenshot="+filepath.Join(abs, s.file))
			t.Logf("снимок: %s", filepath.Join(abs, s.file))
		}
	}
}

// edgeFacts — подставной демон «Интересных фактов» за REST /api/facts/*:
// тела ответов — в форматах инструментов демона (internal/daemon/tools.go),
// коды — по контракту internal/feed. Состояние выбирает адрес страницы
// (Referer): сценарии с «facts-down» видят недоступный демон — status
// отвечает conn=down, остальное 503. Поиск «ошибка» отвечает 422.
type edgeFacts struct {
	mu    sync.Mutex
	added []map[string]any // выпуски, собранные «Собрать выпуск сейчас»
	built int              // сводок, собранных «Собрать сводку»
}

func newEdgeFacts() *edgeFacts { return &edgeFacts{} }

const edgeFactsHint = "запусти animals-mcp -http 127.0.0.1:8766"

func edgeSrc(id, title, url string) map[string]any {
	return map[string]any{"id": id, "title": title, "url": url}
}

func edgeFact(text string, src ...map[string]any) map[string]any {
	return map[string]any{"text": text, "sources": src}
}

// edgeIssues — выпуски ленты, новые первыми. Второй — с разметкой в
// текстах: интерфейс обязан показать её буквами.
func edgeIssues() []map[string]any {
	mdd := edgeSrc("S1", "MDD: Otocolobus manul", "https://www.mammaldiversity.org/taxon/1006010")
	wiki := edgeSrc("S2", "Википедия: Манул", "https://ru.wikipedia.org/wiki/Манул")
	return []map[string]any{
		{"id": 3, "created_at": "2026-09-25T14:00:00+03:00", "species_id": 1006010, "sci_name": "Otocolobus manul",
			"name_ru": "Манул", "iucn": "LC", "status": "ok", "title": "Манул: кошка с круглыми зрачками",
			"lead": "Манул живёт в холодных степях Центральной Азии и почти не умеет быстро бегать.",
			"facts": []map[string]any{
				edgeFact("Зрачки манула остаются круглыми даже на ярком свету.", mdd, wiki),
				edgeFact("Густой мех позволяет ему лежать на снегу и мёрзлой земле.", wiki),
				edgeFact("Ссылка с опасной схемой не должна стать ссылкой.", edgeSrc("S3", "подложный", "javascript:alert(1)")),
			},
			"out_of_range": []string{"Germany", "Japan"}, "cost_usd": 0.0021},
		{"id": 2, "created_at": "2026-09-25T13:00:00+03:00", "species_id": 1001234, "sci_name": "Erinaceus <b>europaeus</b>",
			"name_ru": "Ёж <i>обыкновенный</i>", "iucn": "EN", "status": "thin", "title": "<script>window.__xss=2</script>Ёж",
			"lead": "Вступление с <b>тегом</b>.",
			"facts": []map[string]any{
				edgeFact(`<img src=x onerror="window.__xss=1">Ёж спит всю зиму.`,
					edgeSrc("S1", `"><img src=x onerror=window.__xss=3>`, "https://example.org/?a=<b>")),
			},
			"cost_usd": 0.0017},
		{"id": 1, "created_at": "2026-09-25T12:00:00+03:00", "species_id": 1002000, "sci_name": "Craseonycteris thonglongyai",
			"name_ru": "Свиноносая летучая мышь", "iucn": "NT", "status": "ok", "title": "Самое маленькое млекопитающее",
			"lead": "Весит около двух граммов.",
			"facts": []map[string]any{
				edgeFact("Живёт в известняковых пещерах Таиланда и Мьянмы.", edgeSrc("S1", "MDD", "https://www.mammaldiversity.org/")),
			},
			"cost_usd": 0.0019},
	}
}

func edgeNewIssue(id int) map[string]any {
	return map[string]any{"id": id, "created_at": "2026-09-25T14:40:00+03:00", "species_id": 1003000,
		"sci_name": "Tapirus pinchaque", "name_ru": "Горный тапир", "iucn": "EN", "status": "ok",
		"title": "Горный тапир: шуба для Анд", "lead": "Самый мохнатый из тапиров.",
		"facts":    []map[string]any{edgeFact("Живёт на высоте до 4500 м.", edgeSrc("S1", "MDD", "https://www.mammaldiversity.org/"))},
		"cost_usd": 0.0023}
}

func edgeSummary(id int) map[string]any {
	return map[string]any{"id": id, "from": "2026-09-24T15:00:00+03:00", "to": "2026-09-25T15:00:00+03:00",
		"created_at": "2026-09-25T15:00:05+03:00", "trigger": "schedule",
		"text": fmt.Sprintf("Сводка %d. За сутки — три выпуска: манул, ёж и свиноносая летучая мышь.\n\n"+
			"Один выпуск тонкий: проверяющий отбросил два факта.", id),
		"aggregate": map[string]any{
			"issues":    3,
			"by_status": []map[string]any{{"key": "ok", "count": 2}, {"key": "thin", "count": 1}},
			"species": []map[string]any{
				{"issue_id": 3, "sci_name": "Otocolobus manul", "name_ru": "Манул", "status": "ok", "facts": 3},
				{"issue_id": 1, "sci_name": "Craseonycteris thonglongyai", "status": "ok", "facts": 1}},
			"by_order": []map[string]any{{"key": "Carnivora", "count": 1}, {"key": "Chiroptera", "count": 1}, {"key": "Eulipotyphla", "count": 1}},
			"by_iucn":  []map[string]any{{"key": "LC", "count": 1}, {"key": "NT", "count": 1}, {"key": "EN", "count": 1}},
			"by_realm": []map[string]any{{"key": "Palearctic", "count": 2}},
			"facts":    5, "dropped": 2, "dropped_share": 0.2857, "picks": 4, "rejected": 1,
			"rejected_by_reason":   []map[string]any{{"key": "мало источников", "count": 1}},
			"failures":             []string{"12:30 issue: досье: GBIF не ответил"},
			"out_of_range_species": []string{"Otocolobus manul: Germany, Japan"},
			"cost_usd":             0.0081, "budget_skips": 0},
		"cost": map[string]any{"usd": 0.0009, "tariff": "deepseek", "known": true}}
}

func edgeFullIssue(is map[string]any) map[string]any {
	full := map[string]any{"order": "Carnivora", "family": "Felidae", "realms": []string{"Palearctic"}, "took": "38.1s",
		"dropped": []map[string]any{{"text": "Манул — предок домашней кошки.",
			"sources": []map[string]any{edgeSrc("S2", "Википедия", "https://ru.wikipedia.org/wiki/Манул")},
			"reason":  "источник этого не говорит"}},
		"observations": map[string]any{"total": 1532, "window_days": 365, "recent": 88, "by_country": []map[string]any{
			{"code": "MN", "name": "Mongolia", "count": 900, "range": "in"},
			{"code": "DE", "name": "Germany", "count": 12, "range": "out"}}},
		"spend": []map[string]any{
			{"step": "editor", "model": "deepseek-v4-flash", "requests": 1, "tokens": 5200, "cost_usd": 0.0014, "took": "21.3s"},
			{"step": "verifier", "model": "deepseek-v4-flash", "requests": 1, "tokens": 2600, "cost_usd": 0.0007, "took": "16.8s"}}}
	for k, v := range is {
		full[k] = v
	}
	return full
}

func (f *edgeFacts) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/facts")
	if strings.Contains(r.Referer(), "facts-down") {
		if path == "/status" {
			reply(200, map[string]any{"conn": "down", "server": "http://127.0.0.1:8766",
				"reason": "dial tcp 127.0.0.1:8766: connection refused", "hint": edgeFactsHint, "checked": time.Now()})
			return
		}
		reply(503, map[string]any{"error": "демон «Интересных фактов» не отвечает (127.0.0.1:8766): " + edgeFactsHint})
		return
	}
	if path == "/run" || path == "/summary/build" {
		f.run(w, r, path, reply)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	issues := append(append([]map[string]any{}, f.added...), edgeIssues()...)
	q := r.URL.Query()
	switch path {
	case "/status":
		reply(200, map[string]any{"conn": "ok", "server": "http://127.0.0.1:8766", "version": "animals-mcp 1.8", "checked": time.Now(),
			"schedule": map[string]any{"now": "2026-09-25T14:25:00+03:00", "location": "Europe/Moscow", "budget_usd": 0.5,
				"spent_today_usd": 0.0123, "budget_text": "потрачено $0.0123 из $0.50 за сутки, осталось $0.4877",
				"jobs": []map[string]any{
					{"name": "issue", "every": "1h0m0s", "paid": true, "running": false, "next_text": "25.09.2026 15:00 (через 35 мин)",
						"last_text": "25.09.2026 14:00 (25 мин назад): ok — Манул, 3 факта; $0.0021"},
					{"name": "summary", "daily": "15:00", "paid": true, "running": false, "next_text": "25.09.2026 15:00 (через 35 мин)"},
					{"name": "mdd", "daily": "04:00", "paid": false, "running": false, "next_text": "26.09.2026 04:00 (через 13 ч 35 мин)"},
				}}})
	case "/latest":
		reply(200, map[string]any{"total": len(issues), "returned": len(issues), "issues": issues})
	case "/issue":
		for _, is := range issues {
			if fmt.Sprint(is["id"]) == q.Get("id") {
				reply(200, edgeFullIssue(is))
				return
			}
		}
		reply(422, map[string]any{"error": "выпуска с id " + q.Get("id") + " нет"})
	case "/search":
		text := strings.ToLower(q.Get("text"))
		if text == "ошибка" {
			reply(422, map[string]any{"error": "since: ожидалась дата YYYY-MM-DD или время RFC3339"})
			return
		}
		rows := []map[string]any{}
		for _, is := range issues {
			if strings.Contains(strings.ToLower(fmt.Sprint(is["name_ru"], is["title"], is["sci_name"])), text) {
				rows = append(rows, map[string]any{"id": is["id"], "created_at": is["created_at"], "sci_name": is["sci_name"],
					"name_ru": is["name_ru"], "status": is["status"], "title": is["title"]})
			}
		}
		out := map[string]any{"total": len(rows), "returned": len(rows), "offset": 0, "issues": rows}
		if len(rows) == 0 {
			out["hint"] = "Ничего не найдено."
		}
		reply(200, out)
	case "/summary":
		id := 2 + f.built
		if s := q.Get("id"); s != "" {
			fmt.Sscan(s, &id)
		}
		if id < 1 || id > 2+f.built {
			reply(422, map[string]any{"error": fmt.Sprintf("сводки с id %d нет", id)})
			return
		}
		reply(200, edgeSummary(id))
	case "/summaries":
		rows := []map[string]any{}
		for id := 2 + f.built; id >= 1; id-- {
			s := edgeSummary(id)
			rows = append(rows, map[string]any{"id": id, "from": s["from"], "to": s["to"], "created_at": s["created_at"],
				"trigger": s["trigger"], "text": s["text"]})
		}
		reply(200, map[string]any{"returned": len(rows), "summaries": rows, "hint": "Сводка целиком — summary_get с id."})
	default:
		reply(404, map[string]any{"error": "нет такого пути"})
	}
}

// run — POST /run и /summary/build: запуск идёт секунды, интерфейс должен
// показать ожидание и не дать нажать второй раз.
func (f *edgeFacts) run(w http.ResponseWriter, r *http.Request, path string, reply func(int, any)) {
	if r.Method != http.MethodPost {
		reply(405, map[string]any{"error": "нужен POST"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		reply(400, map[string]any{"error": "тело не JSON: " + err.Error()})
		return
	}
	time.Sleep(700 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if path == "/summary/build" {
		if body["hours"] != float64(24) {
			reply(400, map[string]any{"error": fmt.Sprintf("неожиданное тело %v", body)})
			return
		}
		f.built++
		reply(200, edgeSummary(2+f.built))
		return
	}
	if body["job"] != "issue" {
		reply(400, map[string]any{"error": fmt.Sprintf("неожиданное тело %v", body)})
		return
	}
	id := 4 + len(f.added)
	f.added = append([]map[string]any{edgeNewIssue(id)}, f.added...)
	reply(200, map[string]any{"run": map[string]any{"id": 77, "job": "issue", "trigger": "manual", "status": "ok",
		"ref": fmt.Sprintf("issue:%d", id), "detail": "Горный тапир, 1 факт", "cost_usd": 0.0023, "took": "0.7s"},
		"issue": edgeNewIssue(id)})
}

// edgePipe — подставной REST конвейера за /api/pipeline/: контракт
// internal/feed (pipes.go), шаги двигаются не по часам, а по опросам —
// каждый GET прогона продвигает его на полшага (шаг «идёт», затем «готово»),
// так что сценарий успевает увидеть промежуточные состояния при любой
// скорости виртуального времени headless Edge. Вид «ошибка» обрывает
// цепочку на summarize. В итогах шагов и превью файла — разметка: окно
// обязано показать её буквами.
type edgePipe struct {
	mu   sync.Mutex
	seq  int
	runs []*edgePipeRun
	// posts — тела POST по порядку.
	posts []map[string]any
}

type edgePipeRun struct {
	id    string
	req   map[string]any
	phase int // 0 — все ожидают; 1 — search идёт; 2 — search готов, summarize идёт; …
	start time.Time
}

func newEdgePipe() *edgePipe { return &edgePipe{} }

const edgePipeXSS = `<img src=x onerror="window.__xss=21">`

// edgePipeDigests — отпечатки выходов трёх шагов.
var edgePipeDigests = []string{
	"sha256:1111aaaa2222bbbb3333cccc4444dddd5555eeee6666ffff7777aaaa8888bbbb",
	"sha256:9999cccc0000dddd1111eeee2222ffff3333aaaa4444bbbb5555cccc6666dddd",
	"sha256:7777eeee8888ffff9999aaaa0000bbbb1111cccc2222dddd3333eeee4444ffff",
}

func (r *edgePipeRun) failing() bool { return r.req["query"] == "ошибка" }

// done — прогон кончился: три шага готовы (фаза 6) или summarize упал (4).
func (r *edgePipeRun) done() bool {
	if r.failing() {
		return r.phase >= 4
	}
	return r.phase >= 6
}

func (r *edgePipeRun) view() map[string]any {
	tools := []string{"search", "summarize", "save_to_file"}
	kinds := []string{"dossier", "facts", "file"}
	summaries := []string{"Манул (Otocolobus manul): 9 материалов " + edgePipeXSS, "5 фактов из 6 <b>проверены</b>", "exports/otocolobus-manul.md, 2,1 КБ"}
	steps := []map[string]any{}
	failed := ""
	for i, tool := range tools {
		s := map[string]any{"n": i + 1, "tool": tool, "status": "pending"}
		switch {
		case r.phase > 2*i+1:
			s["status"], s["kind"], s["digest"], s["summary"] = "ok", kinds[i], edgePipeDigests[i], summaries[i]
			s["took"] = int64(1200+i*900) * int64(time.Millisecond)
			s["bytes"] = []int{18432, 4100, 1900}[i]
			checks := []map[string]any{{"name": "отпечаток ответа", "ok": true}, {"name": "вид данных", "ok": true, "note": kinds[i]}}
			if i > 0 {
				checks = append(checks, map[string]any{"name": fmt.Sprintf("вход = выход шага %d", i), "ok": true})
			}
			s["checks"] = checks
			if i == 1 {
				s["cost_usd"] = 0.0021
			}
		case r.phase == 2*i+1:
			s["status"] = "running"
		}
		if i > 0 && s["status"] != "pending" {
			s["input"] = edgePipeDigests[i-1]
		}
		if r.failing() && i == 1 && r.phase >= 4 {
			failed = "summarize: редактор не ответил вовремя <script>window.__xss=24</script>"
			s["status"], s["error"] = "failed", failed
			delete(s, "digest")
			delete(s, "summary")
			delete(s, "checks")
		}
		steps = append(steps, s)
	}
	tr := map[string]any{"request": r.req, "mode": r.req["mode"], "steps": steps, "ok": false,
		"started": r.start, "took": int64(time.Duration(r.phase) * 700 * time.Millisecond), "cost_usd": 0.0}
	if r.phase >= 4 {
		tr["cost_usd"] = 0.0021
	}
	if failed != "" {
		tr["error"] = failed
	} else if r.done() {
		tr["ok"] = true
		tr["file"] = map[string]any{"path": "exports/otocolobus-manul.md", "format": "md", "bytes": 2150,
			"sha256": "5eed5eed0123456789abcdef0123456789abcdef0123456789abcdef01234567",
			"chain":  []string{edgePipeDigests[0], edgePipeDigests[1]},
			"preview": "# Манул <script>window.__xss=22</script>\n\n" + `<img src=x onerror="window.__xss=23">` + "\n\n" +
				"1. Зрачки манула остаются круглыми даже на ярком свету.\n2. Густой мех позволяет ему лежать на снегу."}
	}
	return map[string]any{"id": r.id, "mode": r.req["mode"], "done": r.done(), "trace": tr}
}

func (p *edgePipe) row(r *edgePipeRun) map[string]any {
	v := r.view()
	tr := v["trace"].(map[string]any)
	row := map[string]any{"id": r.id, "query": r.req["query"], "random": r.req["random"], "mode": r.req["mode"],
		"done": r.done(), "ok": tr["ok"], "started": r.start, "took": tr["took"], "cost_usd": tr["cost_usd"]}
	if e, ok := tr["error"]; ok {
		row["error"] = e
	}
	if f, ok := tr["file"].(map[string]any); ok {
		row["file"] = f["path"]
	}
	return row
}

func (p *edgePipe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/pipeline"), "/")
	switch {
	case path == "runs" && r.Method == http.MethodPost:
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			reply(400, map[string]any{"error": "тело запроса не разобралось: " + err.Error()})
			return
		}
		p.posts = append(p.posts, body)
		for _, run := range p.runs {
			if !run.done() {
				reply(409, map[string]any{"error": "конвейер уже идёт (прогон " + run.id + ") — дождитесь его конца"})
				return
			}
		}
		p.seq++
		run := &edgePipeRun{id: fmt.Sprintf("p%d", p.seq), req: body, start: time.Now()}
		p.runs = append(p.runs, run)
		reply(202, map[string]any{"id": run.id})
	case path == "runs":
		rows := []map[string]any{}
		for i := len(p.runs) - 1; i >= 0; i-- {
			rows = append(rows, p.row(p.runs[i]))
		}
		reply(200, map[string]any{"runs": rows})
	case strings.HasPrefix(path, "runs/"):
		id := strings.TrimPrefix(path, "runs/")
		for _, run := range p.runs {
			if run.id == id {
				if !run.done() {
					run.phase++
				}
				reply(200, run.view())
				return
			}
		}
		reply(404, map[string]any{"error": "прогона " + id + " нет"})
	default:
		reply(404, map[string]any{"error": "нет такого раздела"})
	}
}

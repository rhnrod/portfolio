package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/joho/godotenv"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"gopkg.in/yaml.v3"
)

// --- Structs Last.fm ---
type LastFmResponse struct {
	RecentTracks struct {
		Track []Track `json:"track"`
	} `json:"recenttracks"`
}

type Track struct {
	Name   string `json:"name"`
	Artist struct {
		Name string `json:"name"`
		Text string `json:"#text"`
	} `json:"artist"`
	Album struct {
		Name string `json:"name"`
		Text string `json:"#text"`
	} `json:"album"`
	Duration string  `json:"duration"`
	Image    []Image `json:"image"`
	Attr     *Attr   `json:"@attr,omitempty"`
}

type Image struct {
	Size string `json:"size"`
	Text string `json:"#text"`
}

type Attr struct {
	NowPlaying string `json:"nowplaying"`
}

type SongInfo struct {
	Title      string
	Artist     string
	Album      string
	Duration   string
	CoverURL   string
	NowPlaying bool
}

// Instância global do cache (será inicializada no main após carregar o .env)
var lastFmCache *LastFMCache

// --- Estruturas originais ---
type Frontmatter struct {
	Title     string   `yaml:"title"`
	Date      string   `yaml:"date"`
	LastDate  string   `yaml:"last-date"`
	Author    string   `yaml:"author"`
	Thumbnail string   `yaml:"thumbnail"`
	Tags      []string `yaml:"tags"`
}

type Post struct {
	Title       string
	Slug        string
	Date        string
	DateISO     string
	DateTooltip string
	Author      string
	Thumbnail   string
	ReadTime    int
	File        string
	Tags        []string
	Content     template.HTML
}

type TOCItem struct {
	ID    string
	Text  string
	Level int
}

type BlogTag struct {
	Name   string
	Count  int
	Active bool
}

type CalDay struct {
	Day     int
	Count   int
	HasPost bool
	Active  bool
	Future  bool
	DateKey string
	Color   string
}

type CalOption struct {
	Key    string
	Label  string
	URL    string
	Active bool
}

type CalMonth struct {
	MonthName string
	Year      int
	MonthKey  string
	Days      []CalDay
	Months    []CalOption
	Years     []CalOption
	PrevURL   string
	NextURL   string
}

type PageData struct {
	Posts        []Post
	CurrentPost  *Post
	PrevPost     *Post
	NextPost     *Post
	TOC          []TOCItem
	IsReadView   bool
	IsBlogView   bool
	FeaturedPost *Post
	CurrentPage  int
	TotalPages   int
	PrevPage     int
	NextPage     int
	Pages        []int
	Q            string
	ActiveTag    string
	ActiveCal    string
	ActiveDay    string
	Tags         []BlogTag
	AllTags      []BlogTag
	Calendar     *CalMonth
}

type CodeBlockMeta struct {
	Lang  string
	Title string
	Icon  string
}

var mdParser goldmark.Markdown
var codeBlockAttrRegex = regexp.MustCompile("```([a-zA-Z0-9_-]+)(?:\\s+([^\\n]+))?")

func init() {
	mime.AddExtensionType(".ttf", "font/ttf")
	mime.AddExtensionType(".otf", "font/otf")
	mime.AddExtensionType(".woff", "font/woff")
	mime.AddExtensionType(".woff2", "font/woff2")

	mdParser = goldmark.New(
		goldmark.WithExtensions(
			highlighting.NewHighlighting(
				highlighting.WithStyle("github-dark"),
				highlighting.WithGuessLanguage(true),
				highlighting.WithFormatOptions(chromahtml.WithLineNumbers(true)),
			),
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Erro ao carregar o arquivo .env")
	}

	// Inicializa o cache com a API key carregada do ambiente
	lastFmCache = NewLastFMCache(os.Getenv("LASTFM_API_KEY"))

	Server(8080)
}

func Server(port int) {
	PORT := fmt.Sprintf(":%v", port)
	fs := http.FileServer(http.Dir("."))

	// Rota para o widget HTMX
	http.HandleFunc("/api/lastfm-widget", lastFmHandler)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if after, ok := strings.CutPrefix(r.URL.Path, "/posts/"); ok {
			slug := after
			fileBytes, fm, fileName, found := resolvePostFile(slug)
			if !found {
				http.Error(w, "Post não encontrado", http.StatusNotFound)
				return
			}

			_, mdContent := parseFrontmatter(fileBytes)
			metas := parseCodeBlockAttributes(mdContent)
			ctx := parser.NewContext(parser.WithIDs(&slugIDs{values: map[string]bool{}}))
			doc := mdParser.Parser().Parse(text.NewReader(mdContent), parser.WithContext(ctx))
			toc := buildTOC(doc, mdContent)

			var buf bytes.Buffer
			if err := mdParser.Renderer().Render(&buf, mdContent, doc); err != nil {
				http.Error(w, "Erro ao processar Markdown", http.StatusInternalServerError)
				return
			}

			htmlFinal := postProcessCodeBlocks(buf.String(), metas)
			title := fm.Title
			if title == "" {
				title = strings.Title(strings.ReplaceAll(slug, "-", " "))
			}

			dateISO, dateTooltip := formatPostDates(fm.Date)
			post := Post{Title: title, Slug: slug, Date: fm.Date, DateISO: dateISO, DateTooltip: dateTooltip, Author: fm.Author, Thumbnail: fm.Thumbnail, ReadTime: estimateReadTime(mdContent), File: fileName, Tags: fm.Tags, Content: template.HTML(htmlFinal)}

			var prevPost, nextPost *Post
			if all, err := listPosts("posts"); err == nil {
				for i := range all {
					if all[i].File == fileName {
						if i > 0 {
							nextPost = &all[i-1]
						}
						if i < len(all)-1 {
							prevPost = &all[i+1]
						}
						break
					}
				}
			}

			renderTemplate(w, PageData{CurrentPost: &post, PrevPost: prevPost, NextPost: nextPost, TOC: toc, IsReadView: true})
			return
		}

		if r.URL.Path == "/blog" {
			blogHandler(w, r)
			return
		}

		if r.URL.Path != "/" {
			fs.ServeHTTP(w, r)
			return
		}

		posts, _ := getLastThreePosts("posts")
		renderTemplate(w, PageData{Posts: posts, IsReadView: false})
	})

	fmt.Printf("⚡ Serving at http://localhost%v\n", PORT)
	log.Fatal(http.ListenAndServe(PORT, nil))
}

// --- Blog ---
const blogFirstPageSize = 5
const blogPageSize = 6
const blogSidebarTagLimit = 6

var ptBRMonths = [...]string{
	"janeiro", "fevereiro", "março", "abril", "maio", "junho",
	"julho", "agosto", "setembro", "outubro", "novembro", "dezembro",
}

func blogHandler(w http.ResponseWriter, r *http.Request) {
	all, err := listPosts("posts")
	if err != nil {
		http.Error(w, "Erro ao listar posts", http.StatusInternalServerError)
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	tag := strings.TrimSpace(r.URL.Query().Get("tag"))
	month := strings.TrimSpace(r.URL.Query().Get("m"))
	cal := strings.TrimSpace(r.URL.Query().Get("cal"))

	// Agregações a partir do conjunto completo
	tagCounts := map[string]int{}
	dayCounts := map[string]int{}
	for _, p := range all {
		for _, t := range p.Tags {
			tagCounts[t]++
		}
		if k := postDayKey(p); k != "" {
			dayCounts[k]++
		}
	}

	// Filtros: q, tag e dia (m=YYYY-MM-DD).
	// Navegação do calendário (cal=YYYY-MM) NÃO filtra posts.
	qLower := strings.ToLower(q)
	var filtered []Post
	for _, p := range all {
		if qLower != "" && !strings.Contains(strings.ToLower(p.Title), qLower) {
			continue
		}
		if tag != "" && !containsTag(p.Tags, tag) {
			continue
		}
		if len(month) == 10 && postDayKey(p) != month {
			continue
		}
		filtered = append(filtered, p)
	}

	total := len(filtered)
	var totalPages int
	if total <= blogFirstPageSize {
		totalPages = 1
	} else {
		totalPages = 1 + (total-blogFirstPageSize+blogPageSize-1)/blogPageSize
	}

	page := 1
	if pageStr := r.URL.Query().Get("page"); pageStr != "" {
		if n, e := strconv.Atoi(pageStr); e == nil {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	var featured *Post
	var bento []Post
	if page == 1 {
		if total > 0 {
			featured = &filtered[0]
			bEnd := blogFirstPageSize
			if bEnd > total {
				bEnd = total
			}
			if 1 < bEnd {
				bento = filtered[1:bEnd]
			}
		}
	} else {
		start := blogFirstPageSize + (page-2)*blogPageSize
		end := start + blogPageSize
		if end > total {
			end = total
		}
		if start < end {
			bento = filtered[start:end]
		}
	}

	var pages []int
	for i := 1; i <= totalPages; i++ {
		pages = append(pages, i)
	}

	prevPage, nextPage := 0, 0
	if page > 1 {
		prevPage = page - 1
	}
	if page < totalPages {
		nextPage = page + 1
	}

	// Tags em ordem alfabética, em lista simples
	tagNames := make([]string, 0, len(tagCounts))
	for name := range tagCounts {
		tagNames = append(tagNames, name)
	}
	sort.Strings(tagNames)
	var blogTags []BlogTag
	for _, name := range tagNames {
		blogTags = append(blogTags, BlogTag{
			Name:   name,
			Count:  tagCounts[name],
			Active: name == tag,
		})
	}

	// Sidebar mostra no máximo blogSidebarTagLimit tags (as mais usadas, desempate alfabético);
	// o modal "ver mais" lista todas. Ordem determinística.
	shownTags := blogTags
	if len(blogTags) > blogSidebarTagLimit {
		sorted := make([]BlogTag, len(blogTags))
		copy(sorted, blogTags)
		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Count != sorted[j].Count {
				return sorted[i].Count > sorted[j].Count
			}
			return sorted[i].Name < sorted[j].Name
		})
		shownTags = sorted[:blogSidebarTagLimit]
		// Garante que a tag ativa (se houver) aparece na sidebar, sem duplicar.
		for _, bt := range blogTags {
			if !bt.Active {
				continue
			}
			dup := false
			for _, st := range shownTags {
				if st.Name == bt.Name {
					dup = true
					break
				}
			}
			if !dup {
				shownTags[len(shownTags)-1] = bt
			}
			break
		}
	}

	// Calendário: mês exibido = param cal (senão, mês do post mais recente).
	// O filtro de dia (m=YYYY-MM-DD) é independente do mês exibido no calendário.
	displayYear, displayMonth := latestPostYearMonth(all)
	if cal != "" {
		if y, m, ok := parseYearMonth(cal); ok {
			displayYear, displayMonth = y, m
		}
	}

	// Mês mais antigo com post: limite mínimo para a navegação do calendário.
	minKey := time.Now().Format("2006-01")
	for _, p := range all {
		if k := postMonthKey(p); k != "" && k < minKey {
			minKey = k
		}
	}

	activeDay := ""
	if len(month) == 10 {
		activeDay = month
	}
	activeCal := cal
	if activeCal == "" {
		y, m := latestPostYearMonth(all)
		activeCal = fmt.Sprintf("%04d-%02d", y, int(m))
	}

	calendar := buildCalendar(dayCounts, displayYear, displayMonth, month, q, tag, minKey)

	renderTemplate(w, PageData{
		IsBlogView:   true,
		Posts:        bento,
		FeaturedPost: featured,
		CurrentPage:  page,
		TotalPages:   totalPages,
		PrevPage:     prevPage,
		NextPage:     nextPage,
		Pages:        pages,
		Q:            q,
		ActiveTag:    tag,
		ActiveCal:    activeCal,
		ActiveDay:    activeDay,
		Tags:         shownTags,
		AllTags:      blogTags,
		Calendar:     calendar,
	})
}

func postDayKey(p Post) string {
	if len(p.DateISO) >= 10 {
		return p.DateISO[:10]
	}
	return ""
}

func latestPostYearMonth(all []Post) (int, time.Month) {
	latest := ""
	for _, p := range all {
		if k := postMonthKey(p); k != "" && k > latest {
			latest = k
		}
	}
	if y, m, ok := parseYearMonth(latest); ok {
		return y, m
	}
	now := time.Now()
	return now.Year(), now.Month()
}

func parseYearMonth(key string) (int, time.Month, bool) {
	if len(key) != 7 {
		return 0, 0, false
	}
	y, err := strconv.Atoi(key[:4])
	if err != nil {
		return 0, 0, false
	}
	m, err := strconv.Atoi(key[5:])
	if err != nil || m < 1 || m > 12 {
		return 0, 0, false
	}
	return y, time.Month(m), true
}

func buildCalendar(dayCounts map[string]int, year int, month time.Month, activeMonth, q, tag string, minKey string) *CalMonth {
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	daysInMonth := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
	offset := int(first.Weekday())

	now := time.Now()
	todayKey := now.Format("2006-01-02")
	currentMonthKey := now.Format("2006-01")
	currentYear := now.Year()
	currentMonth := now.Month()

	minYear, minMonth := currentYear, currentMonth
	if y, m, ok := parseYearMonth(minKey); ok {
		minYear, minMonth = y, m
	}

	monthKey := fmt.Sprintf("%04d-%02d", year, month)

	// Filtro de dia (m=YYYY-MM-DD) preservado na navegação do calendário.
	dayFilter := ""
	if len(activeMonth) == 10 {
		dayFilter = activeMonth
	}

	var days []CalDay
	for i := 0; i < offset; i++ {
		days = append(days, CalDay{})
	}
	for d := 1; d <= daysInMonth; d++ {
		key := fmt.Sprintf("%04d-%02d-%02d", year, month, d)
		count := dayCounts[key]
		hasPost := count > 0
		future := key > todayKey
		if future {
			// Dias futuros não são navegáveis nem destacados.
			hasPost = false
			count = 0
		}
		days = append(days, CalDay{
			Day:     d,
			Count:   count,
			HasPost: hasPost,
			Active:  key == activeMonth,
			Future:  future,
			DateKey: key,
			Color:   colorForDay(key),
		})
	}
	// Padding fixo para 6 semanas (42 células): altura constante do calendário.
	const calCells = 42
	for len(days) < calCells {
		days = append(days, CalDay{})
	}

	// Seletor de mês: só oferece meses entre o mês do post mais antigo e o mês atual.
	minMonthOpt := 1
	if year == minYear {
		minMonthOpt = int(minMonth)
	}
	maxMonth := 12
	if year == currentYear {
		maxMonth = int(currentMonth)
	}
	var months []CalOption
	for m := minMonthOpt; m <= maxMonth; m++ {
		mk := fmt.Sprintf("%02d", m)
		months = append(months, CalOption{
			Key:    mk,
			Label:  ptBRMonths[m-1],
			Active: m == int(month),
			URL:    blogURL(1, q, tag, dayFilter, fmt.Sprintf("%04d-%02d", year, m)),
		})
	}

	// Seletor de ano: do atual descendo até o ano do post mais antigo.
	// Mantém o mês exibido, sem nunca ultrapassar o mês do post mais antigo
	// nem o mês atual.
	var years []CalOption
	for y := currentYear; y >= minYear; y-- {
		targetMonth := int(month)
		if y == minYear && targetMonth < int(minMonth) {
			targetMonth = int(minMonth)
		}
		if y == currentYear && targetMonth > int(currentMonth) {
			targetMonth = int(currentMonth)
		}
		years = append(years, CalOption{
			Key:    fmt.Sprintf("%d", y),
			Label:  fmt.Sprintf("%d", y),
			Active: y == year,
			URL:    blogURL(1, q, tag, dayFilter, fmt.Sprintf("%04d-%02d", y, targetMonth)),
		})
	}

	prev := time.Date(year, month-1, 1, 0, 0, 0, 0, time.UTC)
	next := time.Date(year, month+1, 1, 0, 0, 0, 0, time.UTC)

	nextURL := ""
	if nextMonthKey := fmt.Sprintf("%04d-%02d", next.Year(), next.Month()); nextMonthKey <= currentMonthKey {
		nextURL = blogURL(1, q, tag, dayFilter, nextMonthKey)
	}

	prevURL := ""
	if prevMonthKey := fmt.Sprintf("%04d-%02d", prev.Year(), prev.Month()); prevMonthKey >= minKey {
		prevURL = blogURL(1, q, tag, dayFilter, prevMonthKey)
	}

	return &CalMonth{
		MonthName: strings.Title(ptBRMonths[month-1]),
		Year:      year,
		MonthKey:  monthKey,
		Days:      days,
		Months:    months,
		Years:     years,
		PrevURL:   prevURL,
		NextURL:   nextURL,
	}
}

// Paleta no tema github-dark; a cor do dia é derivada por hash estável da data.
var githubDarkPalette = []string{
	"#ff7b72", "#ffa657", "#e3b341", "#7ee787",
	"#79c0ff", "#d2a8ff", "#ff85e1", "#a5d6ff",
}

func colorForDay(key string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return githubDarkPalette[int(h)%len(githubDarkPalette)]
}

func postMonthKey(p Post) string {
	if len(p.DateISO) >= 7 {
		return p.DateISO[:7]
	}
	return ""
}

func containsTag(tags []string, target string) bool {
	for _, t := range tags {
		if t == target {
			return true
		}
	}
	return false
}

func blogURL(page int, q, tag, m, cal string) string {
	params := url.Values{}
	if page > 1 {
		params.Set("page", strconv.Itoa(page))
	}
	if q != "" {
		params.Set("q", q)
	}
	if tag != "" {
		params.Set("tag", tag)
	}
	if m != "" {
		params.Set("m", m)
	}
	if cal != "" {
		params.Set("cal", cal)
	}
	if len(params) == 0 {
		return "/blog"
	}
	return "/blog?" + params.Encode()
}

// --- Lógica Last.fm ---
// Cache da lista de músicas recentes: evita re-buscar a API a cada tick de 15s e
// mantém o mapeamento index -> música estável durante a janela do TTL.
var (
	trackListMu    sync.Mutex
	trackListCache []SongInfo
	trackListTime  time.Time
)

const trackListTTL = 2 * time.Minute

func getTrackList(user, key string) ([]SongInfo, bool) {
	trackListMu.Lock()
	defer trackListMu.Unlock()

	now := time.Now()
	if len(trackListCache) > 0 && now.Before(trackListTime.Add(trackListTTL)) {
		return trackListCache, true
	}

	tracks, err := fetchLastFmTracks(user, key)
	if err != nil {
		if len(trackListCache) > 0 {
			fmt.Printf("⚠️ Falha ao atualizar músicas do Last.fm (%v); usando dados em cache\n", err)
			return trackListCache, true
		}
		return nil, false
	}

	if len(tracks) == 0 {
		if len(trackListCache) > 0 {
			return trackListCache, true
		}
		fmt.Println("⚠️ Nenhuma música retornado pelo Last.fm")
		return nil, false
	}

	trackListCache = tracks
	trackListTime = now
	return tracks, true
}

func lastFmHandler(w http.ResponseWriter, r *http.Request) {
	idxStr := r.URL.Query().Get("index")
	index := 0
	if idxStr != "" {
		fmt.Sscanf(idxStr, "%d", &index)
	}

	user := os.Getenv("LASTFM_USER")
	key := os.Getenv("LASTFM_API_KEY")

	if user == "" || key == "" {
		fmt.Println("❌ Erro: LASTFM_USER ou LASTFM_API_KEY não definidos no ambiente/.env")
		http.Error(w, "Configuração do Last.fm ausente", http.StatusInternalServerError)
		return
	}

	tracks, ok := getTrackList(user, key)
	if !ok {
		fmt.Println("❌ Erro ao buscar músicas do Last.fm: sem dados em cache")
		http.Error(w, "Erro ao carregar músicas", http.StatusInternalServerError)
		return
	}

	song := tracks[index%len(tracks)]
	nextIndex := (index + 1) % len(tracks)

	tmpl := `
    <div class="song-card fade-in" 
         hx-get="/api/lastfm-widget?index={{.Next}}" 
         hx-trigger="every 15s" 
         hx-swap="outerHTML">
        <img src="{{.Song.CoverURL}}" width="80" style="border-radius: 4px;">
        <div class="info">
            <div class="title">{{.Song.Title}}</div>
            <div class="album">{{.Song.Album}}</div>
            <div class="artist">{{.Song.Artist}}</div>
        </div>
    </div>`

	t := template.Must(template.New("lastfm").Parse(tmpl))
	t.Execute(w, map[string]interface{}{"Song": song, "Next": nextIndex})
}

func fetchLastFmTracks(username, apiKey string) ([]SongInfo, error) {
	u := url.Values{}
	u.Add("method", "user.getRecentTracks")
	u.Add("user", username)
	u.Add("api_key", apiKey)
	u.Add("format", "json")
	u.Add("limit", "10")
	u.Add("extended", "1")

	resp, err := http.Get("https://ws.audioscrobbler.com/2.0/?" + u.Encode())
	if err != nil {
		return nil, fmt.Errorf("erro de rede: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("status inesperado da API Last.fm: %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	var res LastFmResponse
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return nil, fmt.Errorf("erro ao decodificar resposta: %w", err)
	}

	if len(res.RecentTracks.Track) == 0 {
		return nil, fmt.Errorf("no tracks")
	}

	var songs []SongInfo
	for _, t := range res.RecentTracks.Track {
		artistName := t.Artist.Text
		if artistName == "" {
			artistName = t.Artist.Name
		}
		if artistName == "" {
			artistName = "Artista desconhecido"
		}

		albumName := t.Album.Text
		if albumName == "" {
			albumName = t.Album.Name
		}

		// Pega a imagem que veio no getRecentTracks (se houver)
		img := ""
		for _, image := range t.Image {
			if image.Text != "" {
				img = image.Text
			}
		}

		// --- APLICANDO A OPÇÃO 1: Se álbum ou imagem vierem vazios, busca do Cache/API ---
		if (albumName == "" || albumName == "Álbum desconhecido" || img == "") && lastFmCache != nil {
			cachedAlbum, cachedCover := lastFmCache.GetOrFetch(artistName, t.Name)
			if albumName == "" || albumName == "Álbum desconhecido" {
				if cachedAlbum != "" {
					albumName = cachedAlbum
				}
			}
			if img == "" {
				img = cachedCover
			}
		}

		// Fallbacks finais caso persista vazio
		if albumName == "" {
			albumName = "Álbum desconhecido"
		}
		if img == "" {
			img = "/static/default-cover.png"
		}

		durationStr := "0:00"
		if t.Duration != "" && t.Duration != "0" {
			var sec int
			fmt.Sscanf(t.Duration, "%d", &sec)
			durationStr = fmt.Sprintf("%d:%02d", sec/60, sec%60)
		}

		songs = append(songs, SongInfo{
			Title:      t.Name,
			Artist:     artistName,
			Album:      albumName,
			Duration:   durationStr,
			CoverURL:   img,
			NowPlaying: t.Attr != nil && t.Attr.NowPlaying == "true",
		})
	}

	return songs, nil
}

// --- (O restante do código de Frontmatter, Markdown, TOC e Utilitários permanece igual) ---
func parseFrontmatter(content []byte) (Frontmatter, []byte) {
	var fm Frontmatter
	if !bytes.HasPrefix(content, []byte("---")) {
		return fm, content
	}

	parts := bytes.SplitN(content[3:], []byte("---"), 2)
	if len(parts) < 2 {
		return fm, content
	}

	_ = yaml.Unmarshal(parts[0], &fm)
	return fm, parts[1]
}

var codeBlockRegex = regexp.MustCompile("(?s)```.*?```|`[^`\n]+`")

func estimateReadTime(md []byte) int {
	text := codeBlockRegex.ReplaceAllString(string(md), " ")
	words := len(strings.Fields(text))
	minutes := (words + 199) / 200
	if minutes < 1 {
		minutes = 1
	}
	return minutes
}

func parsePostDate(s string) time.Time {
	layouts := []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	s = strings.TrimSpace(s)
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

func formatPostDates(s string) (string, string) {
	t := parsePostDate(s)
	if t.IsZero() {
		return "", ""
	}
	return t.Format(time.RFC3339), t.Format("02/01/2006 às 15:04")
}

func parseCodeBlockAttributes(mdContent []byte) []CodeBlockMeta {
	matches := codeBlockAttrRegex.FindAllStringSubmatch(string(mdContent), -1)
	var metas []CodeBlockMeta

	for _, m := range matches {
		lang := m[1]
		attrStr := ""
		if len(m) > 2 {
			attrStr = m[2]
		}

		meta := CodeBlockMeta{
			Lang: lang,
		}

		fields := strings.Fields(attrStr)
		for _, f := range fields {
			kv := strings.SplitN(f, "=", 2)
			if len(kv) == 2 {
				key, val := kv[0], strings.Trim(kv[1], `"'`)
				switch key {
				case "title":
					meta.Title = val
				case "icon":
					meta.Icon = val
				}
			}
		}

		metas = append(metas, meta)
	}
	return metas
}

func buildTOC(doc ast.Node, source []byte) []TOCItem {
	var items []TOCItem

	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		heading, ok := n.(*ast.Heading)
		if !ok || heading.Level < 2 {
			return ast.WalkContinue, nil
		}

		textValue := string(heading.Text(source))
		if textValue == "" {
			return ast.WalkContinue, nil
		}

		id, ok := heading.AttributeString("id")
		if !ok {
			return ast.WalkContinue, nil
		}

		items = append(items, TOCItem{
			ID:    string(id.([]byte)),
			Text:  textValue,
			Level: heading.Level,
		})

		return ast.WalkContinue, nil
	})

	return items
}

func slugify(s string) string {
	var b strings.Builder
	prevDash := false

	for _, r := range s {
		r = unicode.ToLower(r)

		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			prevDash = false
			continue
		}

		if base, ok := accentMap[r]; ok {
			b.WriteByte(base)
			prevDash = false
			continue
		}

		if r == ' ' || r == '-' || r == '_' {
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
			continue
		}
	}

	return strings.Trim(b.String(), "-")
}

var accentMap = map[rune]byte{
	'á': 'a', 'à': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
	'ý': 'y', 'ÿ': 'y',
	'ç': 'c', 'ñ': 'n',
}

type slugIDs struct {
	values map[string]bool
}

func (s *slugIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	base := slugify(string(value))
	if base == "" {
		base = "heading"
	}

	if _, ok := s.values[base]; !ok {
		s.values[base] = true
		return []byte(base)
	}

	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if _, ok := s.values[candidate]; !ok {
			s.values[candidate] = true
			return []byte(candidate)
		}
	}
}

func (s *slugIDs) Put(value []byte) {
	s.values[string(value)] = true
}

func resolvePostFile(slug string) ([]byte, Frontmatter, string, bool) {
	files, err := os.ReadDir("posts")
	if err == nil {
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".md") {
				continue
			}
			filePath := filepath.Join("posts", file.Name())
			content, err := os.ReadFile(filePath)
			if err != nil {
				continue
			}
			fm, _ := parseFrontmatter(content)
			if fm.Title != "" && slugify(fm.Title) == slug {
				return content, fm, file.Name(), true
			}
		}
	}

	filePath := filepath.Join("posts", slug+".md")
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, Frontmatter{}, "", false
	}
	fm, _ := parseFrontmatter(content)
	return content, fm, slug + ".md", true
}

func postProcessCodeBlocks(html string, metas []CodeBlockMeta) string {
	reBlock := regexp.MustCompile(`(?s)<pre[^>]*><code.*?</code></pre>`)

	blockIdx := 0
	processed := reBlock.ReplaceAllStringFunc(html, func(match string) string {
		var meta CodeBlockMeta
		if blockIdx < len(metas) {
			meta = metas[blockIdx]
			blockIdx++
		} else {
			return match
		}

		title := meta.Title
		if title == "" {
			if meta.Lang != "" {
				title = meta.Lang + ".file"
			} else {
				title = "code"
			}
		}

		icon := resolveIcon(meta.Icon, meta.Title, meta.Lang)

		iconHtml := fmt.Sprintf(`<img src="/assets/icons/%s.svg" class="code-icon" alt="%s" />`, icon, icon)
		copyBtnHtml := `<button class="code-copy" type="button" aria-label="Copiar código" title="Copiar código"><span class="copy-icon"><svg xmlns="[http://www.w3.org/2000/svg](http://www.w3.org/2000/svg)" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg></span><span class="copy-ok">✓</span></button>`
		titleHtml := fmt.Sprintf(`<div class="code-header">%s<span class="code-title">%s</span>%s</div>`, iconHtml, title, copyBtnHtml)

		return fmt.Sprintf(`<div class="code-card">%s%s</div>`, titleHtml, match)
	})

	return processed
}

func resolveIcon(explicitIcon, title, lang string) string {
	if explicitIcon != "" {
		return explicitIcon
	}

	if title != "" {
		if iconFromTitle := getIconFromFilename(title); iconFromTitle != "" {
			return iconFromTitle
		}
	}

	return getIconForLang(lang)
}

func getIconFromFilename(filename string) string {
	lower := strings.ToLower(filepath.Base(filename))

	switch lower {
	case "go.mod", "go.sum":
		return "go-mod"
	case "package.json", "package-lock.json":
		return "npm"
	case "dockerfile":
		return "docker"
	case "makefile":
		return "makefile"
	}

	ext := filepath.Ext(lower)
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js":
		return "javascript"
	case ".jsx":
		return "react"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "react-typescript"
	case ".html":
		return "html"
	case ".css":
		return "css"
	case ".scss":
		return "scss"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".xml":
		return "xml"
	case ".svg":
		return "svg"
	case ".md", ".markdown":
		return "markdown"
	case ".sh", ".bash", ".zsh":
		return "shell"
	case ".java":
		return "java"
	case ".c":
		return "c"
	case ".h":
		return "c-header"
	case ".cpp", ".cc":
		return "cpp"
	case ".hpp":
		return "cpp-header"
	case ".cs":
		return "cs"
	case ".rs":
		return "rust"
	case ".rb":
		return "ruby"
	case ".php":
		return "php"
	case ".lua":
		return "lua"
	case ".kt":
		return "kotlin"
	case ".swift":
		return "swift"
	case ".scala":
		return "scala"
	case ".dart":
		return "dart"
	case ".pl":
		return "perl"
	case ".vue":
		return "vue"
	case ".svelte":
		return "svelte"
	case ".astro":
		return "astro"
	}

	return ""
}

func getIconForLang(lang string) string {
	mapping := map[string]string{
		"go":         "go",
		"golang":     "go",
		"python":     "python",
		"py":         "python",
		"js":         "javascript",
		"javascript": "javascript",
		"jsx":        "react",
		"ts":         "typescript",
		"typescript": "typescript",
		"tsx":        "react-typescript",
		"html":       "html",
		"css":        "css",
		"scss":       "scss",
		"json":       "json",
		"yaml":       "yaml",
		"yml":        "yaml",
		"toml":       "toml",
		"xml":        "xml",
		"svg":        "svg",
		"md":         "markdown",
		"markdown":   "markdown",
		"bash":       "shell",
		"sh":         "shell",
		"zsh":        "shell",
		"shell":      "shell",
		"console":    "shell",
		"powershell": "powershell",
		"pwsh":       "powershell",
		"java":       "java",
		"c":          "c",
		"cpp":        "cpp",
		"c++":        "cpp",
		"cs":         "cs",
		"csharp":     "cs",
		"rust":       "rust",
		"rs":         "rust",
		"ruby":       "ruby",
		"rb":         "ruby",
		"php":        "php",
		"lua":        "lua",
		"luau":       "luau",
		"kotlin":     "kotlin",
		"swift":      "swift",
		"scala":      "scala",
		"dart":       "dart",
		"perl":       "perl",
		"sql":        "database",
		"docker":     "docker",
		"git":        "git",
		"makefile":   "makefile",
		"make":       "makefile",
		"node":       "node",
		"nix":        "nix",
		"zig":        "zig",
		"vue":        "vue",
		"svelte":     "svelte",
		"astro":      "astro",
		"tailwind":   "tailwind",
		"react":      "react",
		"next":       "next",
		"vite":       "vite",
		"terraform":  "terraform",
		"hcl":        "hcl",
	}
	if icon, ok := mapping[strings.ToLower(lang)]; ok {
		return icon
	}
	return "_file"
}

var indexTmpl = template.Must(template.New("index.html").
	Funcs(template.FuncMap{"blogURL": blogURL, "sub": func(a, b int) int { return a - b }}).
	ParseFiles("index.html"))

func renderTemplate(w http.ResponseWriter, data PageData) {
	indexTmpl.Execute(w, data)
}

func getLastThreePosts(dir string) ([]Post, error) {
	all, err := listPosts(dir)
	if err != nil {
		return nil, err
	}
	if len(all) > 3 {
		all = all[:3]
	}
	return all, nil
}

func listPosts(dir string) ([]Post, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Post{}, nil
		}
		return nil, err
	}

	type fileInfo struct {
		name    string
		modTime int64
	}

	var fileList []fileInfo
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".md") {
			info, err := file.Info()
			if err == nil {
				fileList = append(fileList, fileInfo{
					name:    file.Name(),
					modTime: info.ModTime().Unix(),
				})
			}
		}
	}

	sort.Slice(fileList, func(i, j int) bool {
		if fileList[i].modTime != fileList[j].modTime {
			return fileList[i].modTime > fileList[j].modTime
		}
		return fileList[i].name < fileList[j].name
	})

	var posts []Post
	for _, entry := range fileList {
		fileName := entry.name
		filePath := filepath.Join(dir, fileName)

		content, _ := os.ReadFile(filePath)

		fm, body := parseFrontmatter(content)

		slug := slugify(fm.Title)
		if slug == "" {
			slug = slugify(strings.TrimSuffix(fileName, ".md"))
		}

		title := fm.Title
		if title == "" {
			title = strings.Title(strings.ReplaceAll(slug, "-", " "))
		}

		dateISO, dateTooltip := formatPostDates(fm.Date)

		posts = append(posts, Post{
			Title:       title,
			Slug:        slug,
			Date:        fm.Date,
			File:        fileName,
			Thumbnail:   fm.Thumbnail,
			ReadTime:    estimateReadTime(body),
			DateISO:     dateISO,
			DateTooltip: dateTooltip,
			Tags:        fm.Tags,
		})
	}

	return posts, nil
}

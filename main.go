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
	"strings"
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
	Title     string `yaml:"title"`
	Date      string `yaml:"date"`
	LastDate  string `yaml:"last-date"`
	Author    string `yaml:"author"`
	Thumbnail string `yaml:"thumbnail"`
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
	Content     template.HTML
}

type TOCItem struct {
	ID    string
	Text  string
	Level int
}

type PageData struct {
	Posts       []Post
	CurrentPost *Post
	PrevPost    *Post
	NextPost    *Post
	TOC         []TOCItem
	IsReadView  bool
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
			post := Post{Title: title, Slug: slug, Date: fm.Date, DateISO: dateISO, DateTooltip: dateTooltip, Author: fm.Author, Thumbnail: fm.Thumbnail, ReadTime: estimateReadTime(mdContent), File: fileName, Content: template.HTML(htmlFinal)}

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

// --- Lógica Last.fm ---
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

	tracks, err := fetchLastFmTracks(user, key)
	if err != nil {
		fmt.Printf("❌ Erro ao buscar músicas do Last.fm: %v\n", err)
		http.Error(w, "Erro ao carregar músicas", http.StatusInternalServerError)
		return
	}

	if len(tracks) == 0 {
		fmt.Println("⚠️ Nenhuma música retornado pelo Last.fm")
		return
	}

	song := tracks[index%len(tracks)]
	nextIndex := (index + 1) % len(tracks)

	tmpl := `
    <div class="song-card fade-in" 
         hx-get="/api/lastfm-widget?index={{.Next}}" 
         hx-trigger="every 15s" 
         hx-swap="outerHTML transition:true">
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
	if err != nil || resp.StatusCode != 200 {
		return nil, fmt.Errorf("api error")
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)

	resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

	var res LastFmResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
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
	return t.Format(time.RFC3339), t.Format("02/01/2006 15:04")
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

func renderTemplate(w http.ResponseWriter, data PageData) {
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl.Execute(w, data)
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

		fm, _ := parseFrontmatter(content)

		slug := slugify(fm.Title)
		if slug == "" {
			slug = slugify(strings.TrimSuffix(fileName, ".md"))
		}

		title := fm.Title
		if title == "" {
			title = strings.Title(strings.ReplaceAll(slug, "-", " "))
		}

		posts = append(posts, Post{
			Title: title,
			Slug:  slug,
			Date:  fm.Date,
			File:  fileName,
		})
	}

	return posts, nil
}

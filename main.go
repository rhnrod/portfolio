package main

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"gopkg.in/yaml.v3"
)

// Frontmatter mapeia os dados do cabeçalho do Markdown
type Frontmatter struct {
	Title    string `yaml:"title"`
	Date     string `yaml:"date"`
	LastDate string `yaml:"last-date"`
	Author   string `yaml:"author"`
}

type Post struct {
	Title   string
	Slug    string
	Date    string
	Author  string
	Content template.HTML
}

// TOCItem representa um item da tabela de conteúdos gerada a partir dos títulos do Markdown
type TOCItem struct {
	ID    string
	Text  string
	Level int
}

type PageData struct {
	Posts       []Post
	CurrentPost *Post
	TOC         []TOCItem
	IsReadView  bool
}

// Estrutura auxiliar para metadados da cerca de código
type CodeBlockMeta struct {
	Lang  string
	Title string
	Icon  string
}

var mdParser goldmark.Markdown

// Regex para capturar os parâmetros da linha de código do Markdown (ex: ```go title=go.mod icon=go-mod)
var codeBlockAttrRegex = regexp.MustCompile("```([a-zA-Z0-9_-]+)(?:\\s+([^\\n]+))?")

func init() {
	// Garante que o FileServer sirva fontes com o MIME correto (o Go não mapeia .ttf/.woff por padrão)
	mime.AddExtensionType(".ttf", "font/ttf")
	mime.AddExtensionType(".otf", "font/otf")
	mime.AddExtensionType(".woff", "font/woff")
	mime.AddExtensionType(".woff2", "font/woff2")

	mdParser = goldmark.New(
		goldmark.WithExtensions(
			highlighting.NewHighlighting(
				highlighting.WithStyle("github-dark"),
				highlighting.WithGuessLanguage(true),
				highlighting.WithFormatOptions(
					chromahtml.WithLineNumbers(true),
				),
			),
		),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
}

func main() {
	Server(8080)
}

func Server(port int) {
	PORT := fmt.Sprintf(":%v", port)
	fs := http.FileServer(http.Dir("."))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/posts/") {
			slug := strings.TrimPrefix(r.URL.Path, "/posts/")

			fileBytes, fm, found := resolvePostFile(slug)
			if !found {
				http.Error(w, "Post não encontrado", http.StatusNotFound)
				return
			}

			// 1. Separa Frontmatter do Conteúdo Markdown
			_, mdContent := parseFrontmatter(fileBytes)

			// 2. Extrai os atributos customizados (title, icon) antes do Goldmark filtrar
			metas := parseCodeBlockAttributes(mdContent)

			// 3. Converte o Markdown para HTML usando Goldmark (parse único)
			//    com IDs de heading normalizados (acentos -> letra base)
			ctx := parser.NewContext(parser.WithIDs(&slugIDs{values: map[string]bool{}}))
			doc := mdParser.Parser().Parse(text.NewReader(mdContent), parser.WithContext(ctx))

			// 3.1 Gera a Tabela de Conteúdos a partir dos títulos
			toc := buildTOC(doc, mdContent)

			var buf bytes.Buffer
			if err := mdParser.Renderer().Render(&buf, mdContent, doc); err != nil {
				http.Error(w, "Erro ao processar Markdown", http.StatusInternalServerError)
				return
			}

			// 4. Pós-processa o HTML inserindo a Titlebar conforme as regrinhas
			htmlFinal := postProcessCodeBlocks(buf.String(), metas)

			title := fm.Title
			if title == "" {
				title = strings.Title(strings.ReplaceAll(slug, "-", " "))
			}

			post := Post{
				Title:   title,
				Slug:    slug,
				Date:    fm.Date,
				Author:  fm.Author,
				Content: template.HTML(htmlFinal),
			}

			renderTemplate(w, PageData{CurrentPost: &post, TOC: toc, IsReadView: true})
			return
		}

		if r.URL.Path != "/" {
			if strings.HasSuffix(r.URL.Path, ".ttf") {
				w.Header().Set("Content-Type", "font/ttf")
			}
			fs.ServeHTTP(w, r)
			return
		}

		posts, err := getLastThreePosts("posts")
		if err != nil {
			http.Error(w, "Erro ao carregar posts", http.StatusInternalServerError)
			return
		}

		renderTemplate(w, PageData{Posts: posts, IsReadView: false})
	})

	fmt.Printf("⚡ Serving at http://localhost%v\n", PORT)
	log.Fatal(http.ListenAndServe(PORT, nil))
}

// Extrai o bloco YAML do início do arquivo .md
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

// Extrai linguagem, title e icon das cercas de código no texto bruto do Markdown
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

// buildTOC percorre o AST do documento Markdown coletando os headings (h2+)
// que servirão de âncoras na tabela de conteúdos.
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

// slugify converte um texto em um slug: minúsculas, espaços trocados por "-",
// acentos e tils substituídos pela letra base e demais caracteres descartados.
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

// accentMap translitera letras acentuadas/til para a letra base
var accentMap = map[rune]byte{
	'á': 'a', 'à': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u',
	'ý': 'y', 'ÿ': 'y',
	'ç': 'c', 'ñ': 'n',
}

// slugIDs implementa parser.IDs para gerar as âncoras dos headings
// com a mesma normalização do slugify (acentos viram letra base).
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

// resolvePostFile encontra o arquivo .md correspondente ao slug pedido.
// Primeiro tenta casar com o slug derivado do título (frontmatter);
// em último caso, usa o nome do arquivo (compatibilidade com URLs antigas).
func resolvePostFile(slug string) ([]byte, Frontmatter, bool) {
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
				return content, fm, true
			}
		}
	}

	filePath := filepath.Join("posts", slug+".md")
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, Frontmatter{}, false
	}
	fm, _ := parseFrontmatter(content)
	return content, fm, true
}

// Injeta a Titlebar no HTML gerado pelo Goldmark/Chroma
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

		// 1. Determina o Título
		title := meta.Title
		if title == "" {
			if meta.Lang != "" {
				title = meta.Lang + ".file"
			} else {
				title = "code"
			}
		}

		// 2. Determina o Ícone com base nas regras de prioridade
		icon := resolveIcon(meta.Icon, meta.Title, meta.Lang)

		iconHtml := fmt.Sprintf(`<img src="/assets/icons/%s.svg" class="code-icon" alt="%s" />`, icon, icon)
		copyBtnHtml := `<button class="code-copy" type="button" aria-label="Copiar código" title="Copiar código"><span class="copy-icon"><svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg></span><span class="copy-ok">✓</span></button>`
		titleHtml := fmt.Sprintf(`<div class="code-header">%s<span class="code-title">%s</span>%s</div>`, iconHtml, title, copyBtnHtml)

		return fmt.Sprintf(`<div class="code-card">%s%s</div>`, titleHtml, match)
	})

	return processed
}

// Lógica de Prioridade do Ícone:
// 1. Se "icon" foi explicitado, usa-o;
// 2. Se não foi, mas o "title" é compatível com um valor associado, usa o ícone associado ao title;
// 3. Em último caso, usa o ícone padrão da linguagem.
func resolveIcon(explicitIcon, title, lang string) string {
	// Regra 1
	if explicitIcon != "" {
		return explicitIcon
	}

	// Regra 2
	if title != "" {
		if iconFromTitle := getIconFromFilename(title); iconFromTitle != "" {
			return iconFromTitle
		}
	}

	// Regra 3
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
		return fileList[i].modTime > fileList[j].modTime
	})

	var posts []Post
	limit := 3
	if len(fileList) < limit {
		limit = len(fileList)
	}

	for i := 0; i < limit; i++ {
		fileName := fileList[i].name
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
		})
	}

	return posts, nil
}

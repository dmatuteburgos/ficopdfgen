package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/signintech/gopdf"
)

// -------------------- EXE DIR (WINDOWS FIX) --------------------
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	dir := strings.ReplaceAll(exe, "/", "\\")
	return dir[:strings.LastIndex(dir, "\\")]
}

// -------------------- STYLE XML --------------------
type ReportStyle struct {
	XMLName xml.Name `xml:"ReportStyle"`
	PDF     struct {
		PageSize    string  `xml:"PageSize"`
		Orientation string  `xml:"Orientation"`
		FontSize    float64 `xml:"FontSize"`
		Width       float64 `xml:"Width"`
		Height      float64 `xml:"Height"`
	} `xml:"PDF"`
	Rules []struct {
		Tag  string `xml:"tag,attr"`
		Font string `xml:"font,attr"`
	} `xml:"Rules>Rule"`
}

// -------------------- XML PARSER --------------------
type xmlNode struct {
	XMLName xml.Name
	Content string    `xml:",chardata"`
	Nodes   []xmlNode `xml:",any"`
}

func nodeToMap(n xmlNode) map[string]interface{} {
	if len(n.Nodes) == 0 {
		return map[string]interface{}{n.XMLName.Local: strings.TrimSpace(n.Content)}
	}
	result := make(map[string]interface{})
	for _, child := range n.Nodes {
		childMap := nodeToMap(child)
		key := child.XMLName.Local
		val := childMap[key]
		if existing, ok := result[key]; ok {
			switch e := existing.(type) {
			case []interface{}:
				result[key] = append(e, val)
			default:
				result[key] = []interface{}{e, val}
			}
		} else {
			result[key] = val
		}
	}
	return map[string]interface{}{n.XMLName.Local: result}
}

func parseXMLToMap(xmlBytes []byte) (map[string]interface{}, error) {
	var root xmlNode
	if err := xml.Unmarshal(xmlBytes, &root); err != nil {
		return nil, err
	}
	return map[string]interface{}{root.XMLName.Local: nodeToMap(root)[root.XMLName.Local]}, nil
}

// -------------------- LOAD STYLE --------------------
func loadStyle(path string) (*ReportStyle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s ReportStyle
	return &s, xml.Unmarshal(b, &s)
}

// -------------------- PAGE SIZE --------------------
func pageSize(style *ReportStyle) (float64, float64) {
	const mmToPt = 2.83465
	var w, h float64
	switch strings.ToUpper(style.PDF.PageSize) {
	case "LETTER":
		w, h = 612, 792
	case "CUSTOM":
		w = style.PDF.Width * mmToPt
		h = style.PDF.Height * mmToPt
	default:
		w, h = 595.28, 842
	}
	if strings.ToUpper(style.PDF.Orientation) == "L" {
		return h, w
	}
	return w, h
}

// -------------------- WORD WRAP --------------------
func wrap(pdf *gopdf.GoPdf, text, font string, size, maxW float64) []string {
	pdf.SetFont(font, "", size)
	var lines []string
	line := ""
	words := strings.Fields(text)
	for _, w := range words {
		test := strings.TrimSpace(line + " " + w)
		width, _ := pdf.MeasureTextWidth(test)
		if width > maxW && line != "" {
			lines = append(lines, line)
			line = w
		} else {
			if line == "" {
				line = w
			} else {
				line += " " + w
			}
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// -------------------- STYLE TAG PARSER UTF-8 + <br> --------------------
type part struct {
	Text    string
	Font    string
	NewLine bool
}

func parseStyled(line string, rules map[string]string) []part {
	font := rules["normal"]
	var out []part
	buf := ""
	flush := func(newLine bool) {
		if buf != "" || newLine {
			out = append(out, part{Text: buf, Font: font, NewLine: newLine})
			buf = ""
		}
	}

	for len(line) > 0 {
		switch {
		case strings.HasPrefix(line, "<bold>"):
			flush(false)
			font = rules["bold"]
			line = line[6:]
		case strings.HasPrefix(line, "<italic>"):
			flush(false)
			font = rules["italic"]
			line = line[8:]
		case strings.HasPrefix(line, "</bold>"), strings.HasPrefix(line, "</italic>"):
			flush(false)
			font = rules["normal"]
			line = line[strings.Index(line, ">")+1:]
		case strings.HasPrefix(line, "<br>"):
			flush(true)
			line = line[4:]
		case strings.HasPrefix(line, "<br />"):
			flush(true)
			line = line[6:]
		default:
			r, size := utf8.DecodeRuneInString(line)
			buf += string(r)
			line = line[size:]
		}
	}
	flush(false)
	return out
}

// -------------------- PDF WRITE --------------------
func writeText(pdf *gopdf.GoPdf, content string, style *ReportStyle, rules map[string]string, pageW, pageH float64, startY float64) float64 {
	fontSize := style.PDF.FontSize
	if fontSize == 0 {
		fontSize = 12
	}
	margin := 50.0
	y := startY

	for _, line := range strings.Split(content, "\n") {
		parts := parseStyled(line, rules)
		for _, p := range parts {
			if p.NewLine {
				y += fontSize * 1.5
				continue
			}
			for _, l := range wrap(pdf, p.Text, p.Font, fontSize, pageW-2*margin) {
				if y > pageH-margin {
					pdf.AddPage()
					y = margin
				}
				pdf.SetFont(p.Font, "", fontSize)
				pdf.SetXY(margin, y)
				pdf.Text(l)
				y += fontSize * 1.5
			}
		}
	}
	return y
}

// -------------------- TABLA --------------------
func writeTable(pdf *gopdf.GoPdf, headers []string, rows [][]string, x, y, pageW, pageH float64) float64 {
	colCount := len(headers)
	if colCount == 0 {
		return y
	}
	cellW := (pageW - 100) / float64(colCount)
	cellH := 20.0

	// dibujar headers
	for i, h := range headers {
		pdf.SetFont("Arial", "", 12)
		pdf.SetXY(x+float64(i)*cellW, y)
		pdf.CellWithOption(&gopdf.Rect{W: cellW, H: cellH}, h, gopdf.CellOption{Align: gopdf.Center})
	}
	y += cellH

	// dibujar filas
	for _, row := range rows {
		for i, cell := range row {
			pdf.SetXY(x+float64(i)*cellW, y)
			pdf.CellWithOption(&gopdf.Rect{W: cellW, H: cellH}, fmt.Sprintf("%v", cell), gopdf.CellOption{Align: gopdf.Center})
		}
		y += cellH
		if y > pageH-50 {
			pdf.AddPage()
			y = 50
		}
	}
	return y
}

// -------------------- PDF GENERATION --------------------
func generatePDF(text, reportFolder, output string, style *ReportStyle, data map[string]interface{}) error {
	w, h := pageSize(style)
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: gopdf.Rect{W: w, H: h}})
	pdf.AddPage()

	fontDir := reportFolder + "\\fonts"
	files, err := os.ReadDir(fontDir)
	if err != nil {
		return err
	}

	rules := map[string]string{"normal": "", "bold": "", "italic": ""}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := strings.TrimSuffix(f.Name(), f.Name()[strings.LastIndex(f.Name(), "."):])
		path := fontDir + "\\" + f.Name()
		pdf.AddTTFFont(name, path)
		for _, r := range style.Rules {
			if r.Font == name {
				rules[strings.ToLower(r.Tag)] = name
			}
		}
	}

	y := writeText(&pdf, text, style, rules, w, h, 50)

	// Dibujar tabla si existe
	if tbl, ok := data["Table"].(map[string]interface{}); ok {
		headers := []string{}
		rows := [][]string{}

		if hds, ok := tbl["Headers"].([]interface{}); ok {
			for _, h := range hds {
				headers = append(headers, fmt.Sprintf("%v", h))
			}
		}
		if rws, ok := tbl["Rows"].([]interface{}); ok {
			for _, r := range rws {
				row := []string{}
				if rSlice, ok := r.([]interface{}); ok {
					for _, c := range rSlice {
						row = append(row, fmt.Sprintf("%v", c))
					}
				}
				rows = append(rows, row)
			}
		}
		y = writeTable(&pdf, headers, rows, 50, y, w, h) // ✅ puntero y actualización de y
	}

	return pdf.WritePdf(output)
}

// -------------------- HTTP HANDLER --------------------
func generateHandler(w http.ResponseWriter, r *http.Request) {
	report := strings.TrimSpace(r.URL.Query().Get("reportName"))
	if report == "" {
		http.Error(w, "reportName required", 400)
		return
	}

	base := exeDir() + "\\reports\\" + report
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		http.Error(w, "report directory not found: "+base, 500)
		return
	}

	templateFile := base + "\\template.txt"
	styleFile := base + "\\style.xml"
	outDir := base + "\\output"
	_ = os.MkdirAll(outDir, os.ModePerm)

	tmplBytes, err := os.ReadFile(templateFile)
	if err != nil {
		http.Error(w, "template.txt not found: "+err.Error(), 500)
		return
	}

	style, err := loadStyle(styleFile)
	if err != nil {
		http.Error(w, "style.xml not found: "+err.Error(), 500)
		return
	}

	xmlBytes, _ := io.ReadAll(r.Body)
	data, err := parseXMLToMap(xmlBytes)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	tmpl, _ := template.New("r").Parse(string(tmplBytes))
	var buf bytes.Buffer
	_ = tmpl.Execute(&buf, data)

	out := outDir + "\\" + fmt.Sprintf("%s_%s.pdf", report, time.Now().Format("2006-01-02_15-04-05"))

	if err := generatePDF(buf.String(), base, out, style, data); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Write([]byte("PDF created: " + out))
}

// -------------------- MAIN --------------------
func main() {
	http.HandleFunc("/generate", generateHandler)
	fmt.Println("Report generator running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

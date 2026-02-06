package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/signintech/gopdf"
)

//
// -------------------- EXE DIR (WINDOWS FIX) --------------------
//

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Dir(exe)
}

//
// -------------------- REPORT BASE RESOLVER (FIX) --------------------
//

func resolveReportBase(report string) (string, error) {
	// 1️⃣ junto al ejecutable (BINARIO EN WINDOWS)
	exeBase := filepath.Join(exeDir(), "reports", report)
	if info, err := os.Stat(exeBase); err == nil && info.IsDir() {
		return exeBase, nil
	}

	// 2️⃣ fallback: working directory
	wd, _ := os.Getwd()
	wdBase := filepath.Join(wd, "reports", report)
	if info, err := os.Stat(wdBase); err == nil && info.IsDir() {
		return wdBase, nil
	}

	return "", fmt.Errorf("report directory not found: reports/%s", report)
}

//
// -------------------- STYLE XML --------------------
//

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

//
// -------------------- XML GENERIC PARSER --------------------
//

type xmlNode struct {
	XMLName xml.Name
	Content string    `xml:",chardata"`
	Nodes   []xmlNode `xml:",any"`
}

func nodeToMap(n xmlNode) map[string]interface{} {
	if len(n.Nodes) == 0 {
		return map[string]interface{}{
			n.XMLName.Local: strings.TrimSpace(n.Content),
		}
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

	return map[string]interface{}{
		n.XMLName.Local: result,
	}
}

func parseXMLToMap(xmlBytes []byte) (map[string]interface{}, error) {
	var root xmlNode
	if err := xml.Unmarshal(xmlBytes, &root); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		root.XMLName.Local: nodeToMap(root)[root.XMLName.Local],
	}, nil
}

//
// -------------------- LOAD STYLE --------------------
//

func loadStyle(path string) (*ReportStyle, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s ReportStyle
	return &s, xml.Unmarshal(b, &s)
}

//
// -------------------- PAGE SIZE --------------------
//

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

//
// -------------------- PDF GENERATION --------------------
//

func generatePDF(text, reportFolder, output string, style *ReportStyle) error {
	w, h := pageSize(style)

	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: gopdf.Rect{W: w, H: h}})
	pdf.AddPage()

	fontDir := filepath.Join(reportFolder, "fonts")
	files, err := os.ReadDir(fontDir)
	if err != nil {
		return err
	}

	rules := map[string]string{
		"normal": "",
		"bold":   "",
		"italic": "",
	}

	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := strings.TrimSuffix(f.Name(), filepath.Ext(f.Name()))
		path := filepath.Join(fontDir, f.Name())
		pdf.AddTTFFont(name, path)

		for _, r := range style.Rules {
			if r.Font == name {
				rules[strings.ToLower(r.Tag)] = name
			}
		}
	}

	pdf.SetFont(rules["normal"], "", style.PDF.FontSize)
	pdf.SetXY(50, 50)
	pdf.Text(text)

	return pdf.WritePdf(output)
}

//
// -------------------- HTTP HANDLER --------------------
//

func generateHandler(w http.ResponseWriter, r *http.Request) {
	report := r.URL.Query().Get("reportName")
	if report == "" {
		http.Error(w, "reportName required", 400)
		return
	}

	base, err := resolveReportBase(report)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}

	templateFile := filepath.Join(base, "template.txt")
	styleFile := filepath.Join(base, "style.xml")
	outDir := filepath.Join(base, "output")

	_ = os.MkdirAll(outDir, os.ModePerm)

	tmplBytes, err := os.ReadFile(templateFile)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	style, err := loadStyle(styleFile)
	if err != nil {
		http.Error(w, err.Error(), 500)
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

	out := filepath.Join(
		outDir,
		fmt.Sprintf("%s_%s.pdf", report, time.Now().Format("2006-01-02_15-04-05")),
	)

	if err := generatePDF(buf.String(), base, out, style); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Write([]byte("PDF created: " + out))
}

//
// -------------------- MAIN --------------------
//

func main() {
	http.HandleFunc("/generate", generateHandler)
	fmt.Println("Report generator running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

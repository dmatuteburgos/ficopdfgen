package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/phpdave11/gofpdf"
	"golang.org/x/crypto/ssh"
)

/* ================= CONFIG ================= */

type Config struct {
	SSH struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
	} `json:"ssh"`

	RemoteDirectory     string            `json:"remote_directory"`
	PollIntervalSeconds int               `json:"poll_interval_seconds"`
	FontSize            float64           `json:"font_size"`
	Fonts               map[string]string `json:"fonts"`
	Rules               []Rule            `json:"rules"`
}

type Rule struct {
	Name      string `json:"name"`
	Delimiter string `json:"delimiter"`
	Font      string `json:"font"`
}

/* ================= MAIN ================= */

func main() {
	log.Println("Starting program...")
	cfg := loadConfig("config.json")

	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 5
	}
	if cfg.FontSize <= 0 {
		cfg.FontSize = 11
	}

	// Check fonts exist
	for name, path := range cfg.Fonts {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			log.Fatalf("Font %s not found at path: %s", name, path)
		}
	}

	sshClient := connectSSH(cfg)
	defer sshClient.Close()
	log.Println("Connected to remote host:", cfg.SSH.Host)

	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		log.Println("Polling remote directory:", cfg.RemoteDirectory)
		files, err := listRemoteFiles(sshClient, cfg.RemoteDirectory)
		if err != nil {
			log.Println("Error listing files:", err)
			continue
		}

		if len(files) == 0 {
			log.Println("No files found.")
			continue
		}

		log.Println("Found files:", files)

		var wg sync.WaitGroup
		for _, f := range files {
			ext := strings.ToLower(filepath.Ext(f))
			if ext == ".txt" || ext == ".csv" {
				wg.Add(1)
				go func(file string) {
					defer wg.Done()
					processFile(cfg, sshClient, file)
				}(f)
			}
		}

		wg.Wait()
	}
}

/* ================= SSH ================= */

func connectSSH(cfg Config) *ssh.Client {
	conf := &ssh.ClientConfig{
		User: cfg.SSH.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(cfg.SSH.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", cfg.SSH.Host, cfg.SSH.Port)
	client, err := ssh.Dial("tcp", addr, conf)
	if err != nil {
		log.Fatal("SSH connection failed:", err)
	}
	return client
}

func listRemoteFiles(client *ssh.Client, dir string) ([]string, error) {
	s, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close()

	var out bytes.Buffer
	s.Stdout = &out

	cmd := fmt.Sprintf("ls '%s'", dir)
	if err := s.Run(cmd); err != nil {
		return nil, err
	}
	return strings.Fields(out.String()), nil
}

/* ================= PROCESS ================= */

func processFile(cfg Config, client *ssh.Client, filename string) {
	log.Println("Processing file:", filename)
	remotePath := filepath.Join(cfg.RemoteDirectory, filename)

	data, err := readRemoteFile(client, remotePath)
	if err != nil {
		log.Println("Failed to read remote file:", err)
		return
	}
	log.Printf("Read %d bytes from %s\n", len(data), remotePath)

	pdfName := strings.TrimSuffix(filename, filepath.Ext(filename)) + ".pdf"
	localPDF := filepath.Join(os.TempDir(), pdfName)
	log.Println("Generating PDF at:", localPDF)

	var pdfErr error
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == ".txt" {
		pdfErr = txtToPDF(cfg, data, localPDF)
	} else {
		pdfErr = csvToPDF(cfg, data, localPDF)
	}
	if pdfErr != nil {
		log.Println("Failed to generate PDF:", pdfErr)
		return
	}

	log.Println("Uploading PDF to remote directory:", pdfName)
	if err := uploadPDF(client, cfg.RemoteDirectory, localPDF); err != nil {
		log.Println("Failed to upload PDF:", err)
		return
	}

	log.Println("PDF uploaded successfully:", pdfName)
}

/* ================= REMOTE FILE ================= */

func readRemoteFile(client *ssh.Client, path string) ([]byte, error) {
	s, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close()

	var out bytes.Buffer
	s.Stdout = &out

	cmd := fmt.Sprintf("cat '%s'", path)
	if err := s.Run(cmd); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func uploadPDF(client *ssh.Client, dir, localPDF string) error {
	data, err := os.ReadFile(localPDF)
	if err != nil {
		return err
	}

	s, err := client.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()

	cmd := fmt.Sprintf("cat > '%s/%s'", dir, filepath.Base(localPDF))
	s.Stdin = bytes.NewReader(data)
	return s.Run(cmd)
}

/* ================= PDF ================= */

func loadFonts(pdf *gofpdf.Fpdf, cfg Config) {
	for name, path := range cfg.Fonts {
		log.Println("Loading font:", name, "from", path)
		pdf.AddUTF8Font(name, "", path)
	}
}

/* ===== Rule-driven formatter with escaping ===== */

func writeFormattedLine(pdf *gofpdf.Fpdf, cfg Config, line string, lineHeight float64) {
	x, y := pdf.GetXY()
	currentFont := "normal"
	if _, ok := cfg.Fonts[currentFont]; !ok {
		currentFont = ""
	}

	var buf strings.Builder
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		fontToUse := currentFont
		if _, ok := cfg.Fonts[fontToUse]; !ok {
			fontToUse = "normal"
		}
		pdf.SetFont(fontToUse, "", cfg.FontSize)
		pdf.Write(lineHeight, buf.String())
		buf.Reset()
	}

	i := 0
	for i < len(line) {
		// Escaped delimiter
		escaped := false
		if line[i] == '\\' {
			for _, r := range cfg.Rules {
				d := r.Delimiter
				if i+1+len(d) <= len(line) && line[i+1:i+1+len(d)] == d {
					buf.WriteString(d)
					i += 1 + len(d)
					escaped = true
					break
				}
			}
		}
		if escaped {
			continue
		}

		// Rule match
		matched := false
		for _, r := range cfg.Rules {
			d := r.Delimiter
			if i+len(d) <= len(line) && line[i:i+len(d)] == d {
				flush()
				if currentFont == r.Font {
					currentFont = "normal"
				} else {
					currentFont = r.Font
				}
				i += len(d)
				matched = true
				break
			}
		}
		if matched {
			continue
		}

		buf.WriteByte(line[i])
		i++
	}
	flush()
	pdf.SetXY(x, y+lineHeight)
}

/* ================= TXT ================= */

func txtToPDF(cfg Config, data []byte, output string) error {
	pdf := gofpdf.New("P", "mm", "A4", "")
	loadFonts(pdf, cfg)
	pdf.AddPage()

	lineHeight := 6.0
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		writeFormattedLine(pdf, cfg, line, lineHeight)
	}

	err := pdf.OutputFileAndClose(output)
	if err != nil {
		log.Println("Error writing PDF:", err)
	}
	return err
}

/* ================= CSV ================= */

func csvToPDF(cfg Config, data []byte, output string) error {
	r := csv.NewReader(bytes.NewReader(data))
	records, err := r.ReadAll()
	if err != nil {
		log.Println("CSV read error:", err)
		return err
	}
	if len(records) == 0 {
		log.Println("CSV file empty, skipping PDF generation")
		return nil
	}

	pdf := gofpdf.New("L", "mm", "A4", "")
	loadFonts(pdf, cfg)
	pdf.AddPage()

	lineHeight := 6.0
	colCount := len(records[0])
	colWidth := 270.0 / float64(colCount)

	for _, row := range records {
		xStart, y := pdf.GetXY()
		for i, cell := range row {
			pdf.SetXY(xStart+float64(i)*colWidth, y)
			writeFormattedLine(pdf, cfg, cell, lineHeight)
		}
		pdf.SetXY(xStart, y+lineHeight)
	}

	err = pdf.OutputFileAndClose(output)
	if err != nil {
		log.Println("Error writing CSV PDF:", err)
	}
	return err
}

/* ================= UTIL ================= */

func loadConfig(path string) Config {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal("Config read failed:", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatal("Config parse failed:", err)
	}
	return cfg
}

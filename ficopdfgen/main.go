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

	RemoteDirectory     string `json:"remote_directory"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`

	FontSize float64           `json:"font_size"`
	Fonts    map[string]string `json:"fonts"`
	Rules    []Rule            `json:"rules"`
}

type Rule struct {
	Name      string `json:"name"`
	Delimiter string `json:"delimiter"`
	Font      string `json:"font"`
}

/* ================= MAIN ================= */

func main() {
	cfg := loadConfig("config.json")

	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 5
	}
	if cfg.FontSize <= 0 {
		cfg.FontSize = 11
	}

	sshClient := connectSSH(cfg)
	defer sshClient.Close()

	log.Println("Connected to remote host")

	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		files, err := listRemoteFiles(sshClient, cfg.RemoteDirectory)
		if err != nil {
			log.Println(err)
			continue
		}

		var wg sync.WaitGroup

		for _, f := range files {
			if strings.HasSuffix(f, ".txt") || strings.HasSuffix(f, ".csv") {
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
		log.Fatal(err)
	}
	return client
}

func listRemoteFiles(client *ssh.Client, dir string) ([]string, error) {
	s, _ := client.NewSession()
	defer s.Close()

	var out bytes.Buffer
	s.Stdout = &out

	if err := s.Run("ls " + dir); err != nil {
		return nil, err
	}
	return strings.Fields(out.String()), nil
}

/* ================= PROCESS ================= */

func processFile(cfg Config, client *ssh.Client, filename string) {
	remotePath := filepath.Join(cfg.RemoteDirectory, filename)

	data, err := readRemoteFile(client, remotePath)
	if err != nil {
		log.Println(err)
		return
	}

	pdfName := strings.TrimSuffix(filename, filepath.Ext(filename)) + ".pdf"
	localPDF := filepath.Join(os.TempDir(), pdfName)

	if strings.HasSuffix(filename, ".txt") {
		err = txtToPDF(cfg, data, localPDF)
	} else {
		err = csvToPDF(cfg, data, localPDF)
	}

	if err != nil {
		log.Println(err)
		return
	}

	_ = uploadPDFAndDelete(client, cfg.RemoteDirectory, remotePath, localPDF)
}

func readRemoteFile(client *ssh.Client, path string) ([]byte, error) {
	s, _ := client.NewSession()
	defer s.Close()

	var out bytes.Buffer
	s.Stdout = &out

	if err := s.Run("cat " + path); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func uploadPDFAndDelete(client *ssh.Client, dir, oldFile, localPDF string) error {
	data, _ := os.ReadFile(localPDF)

	s, _ := client.NewSession()
	defer s.Close()

	cmd := fmt.Sprintf(
		"cat > %s/%s && rm %s",
		dir,
		filepath.Base(localPDF),
		oldFile,
	)

	s.Stdin = bytes.NewReader(data)
	return s.Run(cmd)
}

/* ================= PDF ================= */

func loadFonts(pdf *gofpdf.Fpdf, cfg Config) {
	for name, path := range cfg.Fonts {
		pdf.AddUTF8Font(name, "", path)
	}
}

/* ===== Rule-driven formatter with escaping ===== */

func writeFormattedLine(
	pdf *gofpdf.Fpdf,
	cfg Config,
	line string,
	lineHeight float64,
) {
	x, y := pdf.GetXY()
	currentFont := "normal"

	var buf strings.Builder

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		pdf.SetFont(currentFont, "", cfg.FontSize)
		pdf.Write(lineHeight, buf.String())
		buf.Reset()
	}

	for i := 0; i < len(line); {
		// Escaped delimiter
		if line[i] == '\\' {
			for _, r := range cfg.Rules {
				d := r.Delimiter
				if i+1+len(d) <= len(line) && line[i+1:i+1+len(d)] == d {
					buf.WriteString(d)
					i += 1 + len(d)
					goto next
				}
			}
		}

		// Rule match
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
				goto next
			}
		}

		buf.WriteByte(line[i])
		i++

	next:
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

	for _, line := range strings.Split(string(data), "\n") {
		writeFormattedLine(pdf, cfg, line, lineHeight)
	}

	return pdf.OutputFileAndClose(output)
}

/* ================= CSV ================= */

func csvToPDF(cfg Config, data []byte, output string) error {
	r := csv.NewReader(bytes.NewReader(data))
	records, err := r.ReadAll()
	if err != nil {
		return err
	}

	pdf := gofpdf.New("L", "mm", "A4", "")
	loadFonts(pdf, cfg)
	pdf.AddPage()

	lineHeight := 6.0
	colWidth := 270.0 / float64(len(records[0]))

	for _, row := range records {
		xStart, y := pdf.GetXY()

		for i, cell := range row {
			pdf.SetXY(xStart+float64(i)*colWidth, y)
			writeFormattedLine(pdf, cfg, cell, lineHeight)
		}

		pdf.SetXY(xStart, y+lineHeight)
	}

	return pdf.OutputFileAndClose(output)
}

/* ================= UTIL ================= */

func loadConfig(path string) Config {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal(err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatal(err)
	}
	return cfg
}

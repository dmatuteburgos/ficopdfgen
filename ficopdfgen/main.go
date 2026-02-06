package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/phpdave11/gofpdf"
	"github.com/pkg/sftp"
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

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		log.Fatal("Failed to create SFTP client:", err)
	}
	defer sftpClient.Close()
	log.Println("SFTP client ready")

	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		log.Println("Polling remote directory:", cfg.RemoteDirectory)
		files, err := listRemoteFiles(sftpClient, cfg.RemoteDirectory)
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
			ext := strings.ToLower(f[len(f)-4:]) // .txt or .csv
			if ext == ".txt" || ext == ".csv" {
				wg.Add(1)
				go func(file string) {
					defer wg.Done()
					processFile(cfg, sftpClient, file)
				}(f)
			}
		}

		wg.Wait()
	}
}

/* ================= SSH / SFTP ================= */

func connectSSH(cfg Config) *ssh.Client {
	conf := &ssh.ClientConfig{
		User: cfg.SSH.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(cfg.SSH.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", cfg.SSH.Host, cfg.SSH.Port)
	client, err := ssh.Dial("tcp", addr, conf)
	if err != nil {
		log.Fatal("SSH connection failed:", err)
	}
	return client
}

func listRemoteFiles(client *sftp.Client, dir string) ([]string, error) {
	files := []string{}
	remoteFiles, err := client.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, fi := range remoteFiles {
		if !fi.IsDir() {
			files = append(files, fi.Name())
		}
	}
	return files, nil
}

/* ================= PROCESS FILE ================= */

func processFile(cfg Config, sftpClient *sftp.Client, filename string) {
	log.Println("Processing file:", filename)

	pdfName := strings.TrimSuffix(filename, filepath.Ext(filename)) + ".pdf"
	remotePDFPath := cfg.RemoteDirectory + "/" + pdfName

	// Skip if PDF already exists
	if _, err := sftpClient.Stat(remotePDFPath); err == nil {
		log.Println("PDF already exists, skipping:", pdfName)
		return
	}

	remotePath := cfg.RemoteDirectory + "/" + filename
	log.Println("Full remote path:", remotePath)

	data, err := readRemoteFileSFTP(sftpClient, remotePath)
	if err != nil {
		log.Println("Failed to read remote file:", err)
		return
	}
	log.Printf("Read %d bytes from %s\n", len(data), remotePath)

	localPDF := os.TempDir() + "/" + pdfName
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
	if err := uploadPDFSFTP(sftpClient, cfg.RemoteDirectory, localPDF); err != nil {
		log.Println("Failed to upload PDF:", err)
		return
	}

	log.Println("PDF uploaded successfully:", pdfName)
}

/* ================= SFTP FILE OPERATIONS ================= */

func readRemoteFileSFTP(client *sftp.Client, path string) ([]byte, error) {
	f, err := client.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func uploadPDFSFTP(client *sftp.Client, dir, localPDF string) error {
	data, err := os.ReadFile(localPDF)
	if err != nil {
		return err
	}

	remotePath := dir + "/" + filepath.Base(localPDF)
	f, err := client.Create(remotePath)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(data)
	return err
}

/* ================= PDF GENERATION ================= */

func loadFonts(pdf *gofpdf.Fpdf, cfg Config) {
	for name, path := range cfg.Fonts {
		log.Println("Loading font:", name, "from", path)
		pdf.AddUTF8Font(name, "", path)
	}
}

/* ===== Inline formatter with escaped delimiters ===== */

func writeFormattedLineInline(pdf *gofpdf.Fpdf, cfg Config, line string, lineHeight float64) {
	pageWidth, _ := pdf.GetPageSize()
	marginLeft, _, marginRight, _ := pdf.GetMargins()
	maxWidth := pageWidth - marginLeft - marginRight

	xStart, y := pdf.GetXY()
	cursorX := xStart

	currentFont := "normal"
	if _, ok := cfg.Fonts[currentFont]; !ok {
		currentFont = ""
	}
	pdf.SetFont(currentFont, "", cfg.FontSize)

	i := 0
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
		str := buf.String()
		buf.Reset()

		// Measure width
		width := pdf.GetStringWidth(str)
		if cursorX+width > maxWidth {
			// Wrap to next line
			cursorX = marginLeft
			y += lineHeight
			pdf.SetXY(cursorX, y)
		}

		pdf.SetXY(cursorX, y)
		pdf.Write(lineHeight, str)
		cursorX += width
	}

	for i < len(line) {
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
	pdf.SetXY(marginLeft, y+lineHeight)
}

/* ================= TXT ================= */

func txtToPDF(cfg Config, data []byte, output string) error {
	pdf := gofpdf.New("P", "mm", "A4", "")
	loadFonts(pdf, cfg)
	pdf.AddPage()

	lineHeight := 6.0
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		writeFormattedLineInline(pdf, cfg, line, lineHeight)
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
			writeFormattedLineInline(pdf, cfg, cell, lineHeight)
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

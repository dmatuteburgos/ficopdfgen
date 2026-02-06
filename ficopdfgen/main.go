package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/phpdave11/gofpdf"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

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

	PDF struct {
		Orientation string `json:"orientation"` // "P" or "L"
		Unit        string `json:"unit"`        // "mm", "pt", "in"
		PageSize    string `json:"page_size"`   // "A4", "Letter", etc.
	} `json:"pdf"`
}

type Rule struct {
	Name      string `json:"name"`
	Delimiter string `json:"delimiter"`
	Font      string `json:"font"`
}

func main() {
	log.Println("Starting program...")
	cfg := loadConfig("config.json")

	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 5
	}
	if cfg.FontSize <= 0 {
		cfg.FontSize = 11
	}

	for name, path := range cfg.Fonts {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			log.Fatalf("Font %s not found at path: %s", name, path)
		}
	}

	sshClient := connectSSH(cfg)
	defer sshClient.Close()

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		log.Fatal("Failed to create SFTP client:", err)
	}
	defer sftpClient.Close()

	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
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
			ext := strings.ToLower(f[len(f)-4:])
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

// --- SSH/SFTP helpers ---

func connectSSH(cfg Config) *ssh.Client {
	conf := &ssh.ClientConfig{
		User:            cfg.SSH.User,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.SSH.Password)},
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
	var files []string
	entries, err := client.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			files = append(files, e.Name())
		}
	}
	return files, nil
}

func readRemoteFileSFTP(client *sftp.Client, remotePath string) ([]byte, error) {
	f, err := client.Open(remotePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func uploadPDFSFTP(client *sftp.Client, remoteDir, localPDF string) error {
	data, err := os.ReadFile(localPDF)
	if err != nil {
		return err
	}
	remotePath := path.Join(remoteDir, path.Base(localPDF))
	f, err := client.Create(remotePath)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// --- PDF generation ---

func loadFonts(pdf *gofpdf.Fpdf, cfg Config) {
	for name, path := range cfg.Fonts {
		pdf.AddUTF8Font(name, "", path)
	}
}

// Improved line writer: handles long words and wraps properly
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

	words := strings.Fields(line)
	for _, word := range words {
		font := currentFont

		for _, r := range cfg.Rules {
			if strings.HasPrefix(word, r.Delimiter) && strings.HasSuffix(word, r.Delimiter) {
				font = r.Font
				word = word[len(r.Delimiter) : len(word)-len(r.Delimiter)]
			}
		}
		pdf.SetFont(font, "", cfg.FontSize)

		for len(word) > 0 {
			remainingWidth := maxWidth - cursorX
			fit := 0
			for i := 1; i <= len(word); i++ {
				if pdf.GetStringWidth(word[:i]) > remainingWidth {
					break
				}
				fit = i
			}
			if fit == 0 {
				cursorX = marginLeft
				y += lineHeight
				pdf.SetXY(cursorX, y)
				fit = 1
			}

			pdf.SetXY(cursorX, y)
			pdf.Write(lineHeight, word[:fit])
			cursorX += pdf.GetStringWidth(word[:fit])
			word = word[fit:]
			if len(word) > 0 {
				cursorX = marginLeft
				y += lineHeight
				pdf.SetXY(cursorX, y)
			}
		}

		// Add space
		spaceWidth := pdf.GetStringWidth(" ")
		if cursorX+spaceWidth > maxWidth {
			cursorX = marginLeft
			y += lineHeight
		} else {
			cursorX += spaceWidth
		}
		pdf.SetXY(cursorX, y)
	}

	pdf.SetXY(marginLeft, y+lineHeight)
}

func txtToPDF(cfg Config, data []byte, output string) error {
	pdf := gofpdf.New(cfg.PDF.Orientation, cfg.PDF.Unit, cfg.PDF.PageSize, "")
	loadFonts(pdf, cfg)
	pdf.AddPage()

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		writeFormattedLineInline(pdf, cfg, line, 6)
	}

	return pdf.OutputFileAndClose(output)
}

func csvToPDF(cfg Config, data []byte, output string) error {
	r := csv.NewReader(bytes.NewReader(data))
	records, err := r.ReadAll()
	if err != nil || len(records) == 0 {
		return err
	}
	pdf := gofpdf.New(cfg.PDF.Orientation, cfg.PDF.Unit, cfg.PDF.PageSize, "")
	loadFonts(pdf, cfg)
	pdf.AddPage()

	pageWidth, _ := pdf.GetPageSize()
	marginLeft, _, marginRight, _ := pdf.GetMargins()
	usableWidth := pageWidth - marginLeft - marginRight

	lineHeight := 6.0
	colCount := len(records[0])
	colWidth := usableWidth / float64(colCount)

	for _, row := range records {
		xStart, y := pdf.GetXY()
		cursorX := xStart
		for _, cell := range row {
			pdf.SetXY(cursorX, y)
			writeFormattedLineInline(pdf, cfg, cell, lineHeight)
			cursorX += colWidth
		}
		pdf.SetXY(xStart, y+lineHeight)
	}

	return pdf.OutputFileAndClose(output)
}

// --- File processing ---

func processFile(cfg Config, sftpClient *sftp.Client, filename string) {
	log.Println("Processing file:", filename)
	pdfName := strings.TrimSuffix(filename, path.Ext(filename)) + ".pdf"
	remotePDFPath := path.Join(cfg.RemoteDirectory, pdfName)

	if _, err := sftpClient.Stat(remotePDFPath); err == nil {
		log.Println("PDF already exists, skipping:", pdfName)
		return
	}

	remotePath := path.Join(cfg.RemoteDirectory, filename)
	data, err := readRemoteFileSFTP(sftpClient, remotePath)
	if err != nil {
		log.Println("Failed to read remote file:", err)
		return
	}

	localPDF := os.TempDir() + "/" + pdfName
	ext := strings.ToLower(path.Ext(filename))

	go func() {
		var pdfErr error
		if ext == ".txt" {
			pdfErr = txtToPDF(cfg, data, localPDF)
		} else {
			pdfErr = csvToPDF(cfg, data, localPDF)
		}
		if pdfErr != nil {
			log.Println("PDF generation failed:", pdfErr)
			return
		}
		if err := uploadPDFSFTP(sftpClient, cfg.RemoteDirectory, localPDF); err != nil {
			log.Println("Upload failed:", err)
			return
		}
		log.Println("PDF generated and uploaded successfully:", pdfName)
	}()
}

// --- Config loader ---

func loadConfig(path string) Config {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatal("Failed to read config:", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Fatal("Failed to parse config:", err)
	}
	return cfg
}

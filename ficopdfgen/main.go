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

/* ---------------- CONFIG ---------------- */

type Config struct {
	SSH struct {
		Host     string `json:"host"`
		Port     int    `json:"port"`
		User     string `json:"user"`
		Password string `json:"password"`
	} `json:"ssh"`

	RemoteDirectory     string `json:"remote_directory"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`

	Fonts struct {
		Header FontConfig `json:"header"`
		Body   FontConfig `json:"body"`
	} `json:"fonts"`
}

type FontConfig struct {
	Name string  `json:"name"`
	File string  `json:"file"`
	Size float64 `json:"size"`
}

/* ---------------- MAIN ---------------- */

func main() {
	cfg := loadConfig("config.json")

	if cfg.PollIntervalSeconds <= 0 {
		cfg.PollIntervalSeconds = 5
	}

	sshClient := connectSSH(cfg)
	defer sshClient.Close()

	log.Println("Connected to remote host")

	ticker := time.NewTicker(time.Duration(cfg.PollIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		files, err := listRemoteFiles(sshClient, cfg.RemoteDirectory)
		if err != nil {
			log.Println("List error:", err)
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

/* ---------------- SSH ---------------- */

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
	s, _ := client.NewSession()
	defer s.Close()

	var out bytes.Buffer
	s.Stdout = &out

	if err := s.Run("ls " + dir); err != nil {
		return nil, err
	}
	return strings.Fields(out.String()), nil
}

/* ---------------- PROCESSING ---------------- */

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
		log.Println("PDF error:", err)
		return
	}

	err = uploadPDFAndDelete(client, cfg.RemoteDirectory, remotePath, localPDF)
	if err != nil {
		log.Println("Upload error:", err)
	}
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

	cmd := fmt.Sprintf("cat > %s/%s && rm %s",
		dir,
		filepath.Base(localPDF),
		oldFile,
	)

	s.Stdin = bytes.NewReader(data)
	return s.Run(cmd)
}

/* ---------------- PDF CONVERSION ---------------- */

func loadFonts(pdf *gofpdf.Fpdf, cfg Config) {
	pdf.AddUTF8Font(cfg.Fonts.Header.Name, "", cfg.Fonts.Header.File)
	pdf.AddUTF8Font(cfg.Fonts.Body.Name, "", cfg.Fonts.Body.File)
}

func txtToPDF(cfg Config, data []byte, output string) error {
	pdf := gofpdf.New("P", "mm", "A4", "")
	loadFonts(pdf, cfg)

	pdf.AddPage()
	pdf.SetFont(cfg.Fonts.Header.Name, "", cfg.Fonts.Header.Size)
	pdf.Cell(0, 10, "Text Document")
	pdf.Ln(12)

	pdf.SetFont(cfg.Fonts.Body.Name, "", cfg.Fonts.Body.Size)

	for _, line := range strings.Split(string(data), "\n") {
		pdf.MultiCell(0, 7, line, "", "", false)
	}

	return pdf.OutputFileAndClose(output)
}

func csvToPDF(cfg Config, data []byte, output string) error {
	r := csv.NewReader(bytes.NewReader(data))
	records, err := r.ReadAll()
	if err != nil {
		return err
	}

	pdf := gofpdf.New("L", "mm", "A4", "")
	loadFonts(pdf, cfg)

	pdf.AddPage()
	pdf.SetFont(cfg.Fonts.Header.Name, "", cfg.Fonts.Header.Size)
	pdf.Cell(0, 10, "CSV Document")
	pdf.Ln(12)

	pdf.SetFont(cfg.Fonts.Body.Name, "", cfg.Fonts.Body.Size)

	colWidth := 270.0 / float64(len(records[0]))

	for _, row := range records {
		for _, col := range row {
			pdf.CellFormat(colWidth, 7, col, "1", 0, "", false, 0, "")
		}
		pdf.Ln(-1)
	}

	return pdf.OutputFileAndClose(output)
}

/* ---------------- UTIL ---------------- */

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

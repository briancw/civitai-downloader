package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	chunkSize      = 4194304
	userAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/58.0.3029.110 Safari/537.3"
	defaultEnvName = "CIVITAI_TOKEN"
	civitaiBaseURL = "https://civitai.com/api/download/models"
)

func main() {
	// Parse command line arguments
	if len(os.Args) < 3 {
		fmt.Println("Usage: civit-dl <model_id> <output_path>")
		fmt.Println("  model_id: CivitAI Model ID")
		fmt.Println("  output_path: Output directory path (e.g., /workspace/models)")
		os.Exit(1)
	}

	modelID := os.Args[1]
	outputPath := os.Args[2]

	token := getToken()
	if token == "" {
		token = promptForCivitaiToken()
	}

	if err := downloadFile(modelID, outputPath, token); err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
}

func getToken() string {
	token := os.Getenv(defaultEnvName)
	if token != "" {
		return token
	}

	usr, err := user.Current()
	if err != nil {
		return ""
	}

	tokenFile := filepath.Join(usr.HomeDir, ".civit-dl", "config")
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}

func storeToken(token string) {
	usr, err := user.Current()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Could not get current user: %v\n", err)
		return
	}

	configDir := filepath.Join(usr.HomeDir, ".civit-dl")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Could not create config directory: %v\n", err)
		return
	}

	tokenFile := filepath.Join(configDir, "config")
	if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Could not write token file: %v\n", err)
	}

	fmt.Printf("Token stored in: %s\n", tokenFile)
}

func promptForCivitaiToken() string {
	fmt.Print("Please enter your CivitAI API token: ")
	reader := bufio.NewReader(os.Stdin)
	token, err := reader.ReadString('\n')
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: Could not read token: %v\n", err)
		os.Exit(1)
	}
	token = strings.TrimSpace(token)
	storeToken(token)
	return token
}

func downloadFile(modelID, outputPath, token string) error {
	downloadURL := fmt.Sprintf("%s/%s?token=%s", civitaiBaseURL, modelID, url.QueryEscape(token))

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP error: %s", resp.Status)
	}

	var finalResp *http.Response
	var filename string

	switch resp.StatusCode {
	case 301, 302, 303, 307, 308:
		redirectURL := resp.Header.Get("Location")
		if redirectURL == "" {
			return fmt.Errorf("no redirect URL found")
		}

		// Handle relative redirects
		if strings.HasPrefix(redirectURL, "/") {
			parsedBaseURL, _ := url.Parse(downloadURL)
			redirectURL = fmt.Sprintf("%s://%s%s", parsedBaseURL.Scheme, parsedBaseURL.Host, redirectURL)
		}

		// Follow redirect
		finalResp, err = http.Get(redirectURL)
		if err != nil {
			return fmt.Errorf("failed to follow redirect: %w", err)
		}
		defer finalResp.Body.Close()

		if finalResp.StatusCode == 404 {
			return fmt.Errorf("file not found")
		}

		if finalResp.StatusCode >= 400 {
			return fmt.Errorf("redirect failed with status: %d", finalResp.StatusCode)
		}

		resp = finalResp
	case 404:
		return fmt.Errorf("file not found")
	default:
		return fmt.Errorf("no redirect found, status code: %d", resp.StatusCode)
	}

	// Get filename from Content-Disposition header
	contentDisposition := resp.Header.Get("Content-Disposition")
	if contentDisposition != "" && strings.Contains(contentDisposition, "filename=") {
		re := regexp.MustCompile(`filename="?([^";]+)"?`)
		matches := re.FindStringSubmatch(contentDisposition)
		if len(matches) > 1 {
			filename, _ = url.QueryUnescape(matches[1])
		}
	}

	// Fallback: extract filename from URL path
	if filename == "" {
		parsedURL, _ := url.Parse(resp.Request.URL.String())
		path := parsedURL.Path
		if path != "" && strings.Contains(path, "/") {
			filename = filepath.Base(path)
		}
	}

	// Get total size
	totalSize := resp.Header.Get("Content-Length")
	var totalSizeInt int64
	if totalSize != "" {
		totalSizeInt, _ = strconv.ParseInt(totalSize, 10, 64)
	}

	outputFile := filepath.Join(outputPath, filename)

	// Create output directory if it doesn't exist
	if err := os.MkdirAll(outputPath, 0755); err != nil {
		return fmt.Errorf("failed to create output directory: %w", err)
	}

	var downloaded int64

	file, err := os.Create(outputFile)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer file.Close()

	for {
		buffer := make([]byte, chunkSize)
		n, err := resp.Body.Read(buffer)

		if n > 0 {
			if _, writeErr := file.Write(buffer[:n]); writeErr != nil {
				return fmt.Errorf("failed to write to file: %w", writeErr)
			}
			downloaded += int64(n)

			if totalSizeInt > 0 {
				progress := float64(downloaded) / float64(totalSizeInt)
				fmt.Printf("\rDownloading: %s [%.2f%%]", filename, progress*100)
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("error reading response body: %w", err)
		}
	}

	fmt.Printf("\nDownload completed. File saved as: %s\n", filename)

	return nil
}

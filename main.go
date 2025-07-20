package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/fatih/color"
)

type Config struct {
	FiveNWebhook          string `json:"5nWebhook"`
	FiveLWebhook          string `json:"5lWebhook"`
	FourCWebhook          string `json:"4cWebhook"`
	FourLWebhook          string `json:"4lWebhook"`
	ThreeNWebhook         string `json:"3nWebhook"`
	Pronouncable5LWebhook string `json:"Pronouncable5LWebhook"`
}

var (
	config      Config
	checked     = make(map[string]struct{})
	checkedLock sync.Mutex
	proxies     []string

	// Counters using atomic for safe concurrent access
	totalChecked  uint64
	totalValid    uint64
	totalTaken    uint64
	totalCensored uint64

	startTime time.Time
)

func init() {
	rand.Seed(time.Now().UnixNano())
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

// Clear screen function, cross platform
func clearScreen() {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("cmd", "/c", "cls")
		cmd.Stdout = os.Stdout
		cmd.Run()
	default:
		cmd := exec.Command("clear")
		cmd.Stdout = os.Stdout
		cmd.Run()
	}
}

func loadConfig(path string) Config {
	file, err := os.Open(path)
	if err != nil {
		color.Red("Failed to open config.json: %v", err)
	}
	defer file.Close()

	var cfg Config
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&cfg); err != nil {
		color.Red("Failed to decode config.json: %v", err)
	}
	return cfg
}

func loadProxies(path string) []string {
	file, err := os.Open(path)
	if err != nil {
		color.Red("Failed to open proxies.txt: %v", err)
	}
	defer file.Close()

	var loaded []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			if !strings.HasPrefix(line, "http://") && !strings.HasPrefix(line, "https://") {
				line = "http://" + line
			}
			loaded = append(loaded, line)
		}
	}
	if len(loaded) == 0 {
		color.Yellow("No proxies loaded.")
	}
	return loaded
}

func newHttpClientWithProxy(proxyAddr string) *http.Client {
	proxyURL, err := url.Parse(proxyAddr)
	if err != nil {
		color.Red("Invalid proxy: %s — %v (Skipping this one)", proxyAddr, err)
		return http.DefaultClient
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		color.Red("Failed to create cookie jar: %v", err)
		return http.DefaultClient
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        100,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   false,
	}

	return &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   30 * time.Second,
	}
}

func gen5N() string {
	numbers := "0123456789"
	result := make([]byte, 5)
	for i := range result {
		result[i] = numbers[rand.Intn(len(numbers))]
	}
	return string(result)
}

func gen5L() string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	result := make([]rune, 5)
	result[0] = unicode.ToLower(rune(letters[rand.Intn(len(letters))]))
	for i := 1; i < 5; i++ {
		result[i] = rune(letters[rand.Intn(len(letters))])
	}
	return string(result)
}

func gen4Mixed() string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	digits := "0123456789"

	format := rand.Intn(3)

	var parts []byte
	switch format {
	case 0:
		parts = []byte{
			letters[rand.Intn(len(letters))],
			digits[rand.Intn(len(digits))],
			letters[rand.Intn(len(letters))],
			letters[rand.Intn(len(letters))],
		}
	case 1:
		parts = []byte{
			digits[rand.Intn(len(digits))],
			letters[rand.Intn(len(letters))],
			letters[rand.Intn(len(letters))],
			digits[rand.Intn(len(digits))],
		}
	case 2:
		parts = []byte{
			letters[rand.Intn(len(letters))],
			letters[rand.Intn(len(letters))],
			letters[rand.Intn(len(letters))],
			digits[rand.Intn(len(digits))],
		}
	}

	return string(parts)
}

func gen4L() string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	result := make([]byte, 4)
	for i := range result {
		result[i] = letters[rand.Intn(len(letters))]
	}
	return string(result)
}

func gen3N() string {
	numbers := "0123456789"
	result := make([]byte, 3)
	for i := range result {
		result[i] = numbers[rand.Intn(len(numbers))]
	}
	return string(result)
}

// Worker for generated usernames
func worker(generatorFunc func() string, httpClient *http.Client, webhookURL string) {
	for {
		username := generatorFunc()

		checkedLock.Lock()
		if _, exists := checked[username]; exists {
			checkedLock.Unlock()
			continue
		}
		checked[username] = struct{}{}
		checkedLock.Unlock()

		processUsername(username, webhookURL, httpClient)
	}
}

// Worker for reading usernames from slice (file mode)
func workerFromSlice(usernames []string, jobs <-chan int, httpClient *http.Client, webhookURL string, wg *sync.WaitGroup) {
	defer wg.Done()
	for idx := range jobs {
		username := usernames[idx]

		checkedLock.Lock()
		if _, exists := checked[username]; exists {
			checkedLock.Unlock()
			continue
		}
		checked[username] = struct{}{}
		checkedLock.Unlock()

		processUsername(username, webhookURL, httpClient)
	}
}

// Common username processing
func processUsername(username, webhookURL string, client *http.Client) {
	url := fmt.Sprintf("https://auth.roblox.com/v1/usernames/validate?Username=%s&Birthday=2000-01-01", username)

	var resp *http.Response
	var err error
	for i := 0; i < 3; i++ {
		resp, err = client.Get(url)
		if err == nil && resp != nil && resp.StatusCode == http.StatusOK {
			break
		}
		time.Sleep(time.Duration(1<<i) * time.Second)
	}
	if err != nil || resp == nil {
		atomic.AddUint64(&totalChecked, 1)
		return
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		atomic.AddUint64(&totalChecked, 1)
		return
	}

	codeFloat, ok := result["code"].(float64)
	if !ok {
		atomic.AddUint64(&totalChecked, 1)
		return
	}
	code := int(codeFloat)

	atomic.AddUint64(&totalChecked, 1)

	switch code {
	case 0: // Valid
		atomic.AddUint64(&totalValid, 1)
		// Save to valid.txt
		if f, err := os.OpenFile("valid.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			f.WriteString(username + "\n")
			f.Close()
		}

		sendToWebhook(username, webhookURL, nil)

	case 1: // Taken
		atomic.AddUint64(&totalTaken, 1)

	case 2: // Censored
		atomic.AddUint64(&totalCensored, 1)
	}
}

func sendToWebhook(username, webhookURL string, _ *http.Client) {
	payload := map[string]string{
		"content": fmt.Sprintf("Claimable! `%s`", username),
	}
	jsonData, _ := json.Marshal(payload)

	for attempt := 1; attempt <= 5; attempt++ {
		req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(jsonData))
		if err != nil {
			//color.Red("Webhook request creation failed (attempt %d): %v", attempt, err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			//log.Printf("Webhook send failed for %s (attempt %d): %v", username, attempt, err)
			time.Sleep(time.Duration(200+rand.Intn(300)) * time.Millisecond)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {

			return
		}

		//color.Red("Webhook failed for %s (attempt %d): HTTP %d - %s", username, attempt, resp.StatusCode, string(body))

		if resp.StatusCode == 429 {

			retryAfter := resp.Header.Get("Retry-After")
			delay := 3 * time.Second
			if retryAfter != "" {
				if secs, err := strconv.Atoi(retryAfter); err == nil {
					delay = time.Duration(secs) * time.Second
				}
			}
			time.Sleep(delay)
		} else {
			time.Sleep(time.Duration(300+rand.Intn(300)) * time.Millisecond)
		}
	}

	//color.Red("Failed to send webhook for: %s", username)
}

func updateTitle() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for range ticker.C {
		elapsed := time.Since(startTime).Minutes()
		if elapsed == 0 {
			elapsed = 1.0 / 60.0 // avoid divide by zero
		}
		checkedCount := atomic.LoadUint64(&totalChecked)
		validCount := atomic.LoadUint64(&totalValid)
		takenCount := atomic.LoadUint64(&totalTaken)
		censoredCount := atomic.LoadUint64(&totalCensored)

		title := fmt.Sprintf("Speedy Sniper V2 | Checked: %d | Valid: %d | Taken: %d | Censored: %d",
			checkedCount, validCount, takenCount, censoredCount)
		fmt.Printf("\033]0;%s\007", title)
	}
}

func setConsoleTitle(title string) {
	switch runtime.GOOS {
	case "windows":
		cmd := exec.Command("cmd", "/c", "title "+title)
		cmd.Run()
	default:
		// Unix-like terminals usually don't support setting window title this way,
		// but we can try escape sequences:
		fmt.Printf("\033]0;%s\007", title)
	}
}

func printBanner() {
	lines := []string{
		"   _____                     __         ",
		"  / ___/____  ___  ___  ____/ /_  __    ",
		"  \\__ \\/ __ \\/ _ \\/ _ \\/ __  / / / /    ",
		" ___/ / /_/ /  __/  __/ /_/ / /_/ /     ",
		"/____/ .___/\\___/\\___/\\__,_/\\__, /      ",
		"    /_/                    /____/       ",
	}

	// Fade blue colors using ANSI escape codes
	colors := []string{
		"\033[38;5;81m", // light blue
		"\033[38;5;75m", // medium blue
		"\033[38;5;69m", // slate blue
		"\033[38;5;63m", // darker blue
		"\033[38;5;60m", // deeper blue
		"\033[38;5;57m", // navyish
	}

	for i, line := range lines {
		color := colors[i%len(colors)]
		fmt.Println(color + line + "\033[0m")
	}
}

func main() {
	// ANSI color codes
	blue := "\033[34m"
	white := "\033[37m"
	fmt.Println("")
	printBanner()
	fmt.Println("")
	config = loadConfig("config.json")
	proxies = loadProxies("proxies.txt")

	var mode string

	fmt.Println("Select mode:")
	fmt.Println()

	fmt.Println(fmt.Sprintf("%s[ %s1%s ]%s : 5N", blue, white, blue, white))
	fmt.Println(fmt.Sprintf("%s[ %s2%s ]%s : 5L", blue, white, blue, white))
	fmt.Println(fmt.Sprintf("%s[ %s3%s ]%s : 4C", blue, white, blue, white))
	fmt.Println(fmt.Sprintf("%s[ %s4%s ]%s : 4L", blue, white, blue, white))
	fmt.Println(fmt.Sprintf("%s[ %s5%s ]%s : 3N", blue, white, blue, white))
	fmt.Println(fmt.Sprintf("%s[ %s6%s ]%s : Check from file", blue, white, blue, white))

	fmt.Println()
	fmt.Print(">")
	fmt.Scanln(&mode)

	validModes := map[string]func() string{
		"1": gen5N,
		"2": gen5L,
		"3": gen4Mixed,
		"4": gen4L,
		"5": gen3N,
	}

	webhookMap := map[string]string{
		"1": config.FiveNWebhook,
		"2": config.FiveLWebhook,
		"3": config.FourCWebhook,
		"4": config.FourLWebhook,
		"5": config.ThreeNWebhook,
		"6": config.Pronouncable5LWebhook,
	}

	var usernames []string
	var generatorFunc func() string
	var webhookURL string

	// Ask for usernames file for all modes now as you requested:
	fmt.Print("Enter name for file with usernames: ")
	var usernamesFile string
	fmt.Scanln(&usernamesFile)

	// Load usernames if mode 6 (file input)
	if mode == "6" {
		file, err := os.Open(usernamesFile)
		if err != nil {
			color.Red("Failed to open usernames file: %v", err)
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				usernames = append(usernames, line)
			}
		}
		if err := scanner.Err(); err != nil {
			color.Red("Error reading usernames file: %v", err)
		}

		if len(usernames) == 0 {
			color.Red("No usernames loaded from file.")
		}

		// Set webhook to 5L webhook always for mode 6
		webhookURL = config.Pronouncable5LWebhook
		if webhookURL == "" {
			color.Red("5L webhook URL missing in config for mode 6.")
		}

	} else {
		// Other modes: validate mode, set generator and webhook
		genFunc, ok := validModes[mode]
		if !ok {
			color.Red("Invalid mode selected. Defaulting to 5N.")
			mode = "1"
			genFunc = gen5N
		}
		generatorFunc = genFunc

		webhookURL = webhookMap[mode]
		if webhookURL == "" {
			color.Red("Webhook URL not found or empty for mode %s", mode)
		}

		// For non-file modes, optionally load usernames from file as well (your request)
		// Let's load usernames file but only for stats? Or ignore loaded usernames in these modes?
		// You requested to ask username file always, but you didn't clarify use for non-6 modes.
		// So for non-6 modes, ignore usernames file content for checking. We'll only check generator.
	}

	// Threads prompt
	var numThreads int
	fmt.Print("Enter number of threads: ")
	_, err := fmt.Scan(&numThreads)
	if err != nil || numThreads < 1 {
		color.Red("Invalid number. Defaulting to 1 thread.")
		numThreads = 1
	}

	// Clear screen and print status
	clearScreen()
	linesLoaded := len(usernames)
	if mode != "6" {
		// For generated modes, linesLoaded is unknown; just show 0 or "N/A"
		linesLoaded = 0
	}

	printBanner()
	fmt.Println("")

	// Print the quote
	fmt.Println(fmt.Sprintf("%s[ %sQuote%s ]%s - Hunt the bag, Not her.\n", blue, white, blue, white))

	// Print module info with colored "[ ∞ ]"
	fmt.Println(fmt.Sprintf("%s[ %s∞%s ]%s : Module: %s", blue, white, blue, white, modeToName(mode)))
	fmt.Println(fmt.Sprintf("%s[ %s∞%s ]%s : Lines Loaded: %d", blue, white, blue, white, linesLoaded))
	fmt.Println(fmt.Sprintf("%s[ %s∞%s ]%s : Proxies Loaded: %d", blue, white, blue, white, len(proxies)))
	fmt.Println(fmt.Sprintf("%s[ %s∞%s ]%s : Threads Loaded: %d\n", blue, white, blue, white, numThreads))

	// Start title updater
	go updateTitle()

	if mode == "6" {
		// Use channel job queue for slices
		jobs := make(chan int, len(usernames))
		var wg sync.WaitGroup

		for i := 0; i < numThreads; i++ {
			proxy := proxies[i%len(proxies)]
			client := newHttpClientWithProxy(proxy)
			wg.Add(1)
			go workerFromSlice(usernames, jobs, client, webhookURL, &wg)
		}

		for i := range usernames {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
	} else {
		// Generated mode workers
		for i := 0; i < numThreads; i++ {
			proxy := proxies[i%len(proxies)]
			client := newHttpClientWithProxy(proxy)
			go worker(generatorFunc, client, webhookURL)
		}
		select {} // run forever
	}
}

func modeToName(mode string) string {
	switch mode {
	case "1":
		return "5N"
	case "2":
		return "5L"
	case "3":
		return "4C"
	case "4":
		return "4L"
	case "5":
		return "3N"
	case "6":
		return "Check from file"
	default:
		return "Unknown"
	}
}

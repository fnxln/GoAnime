package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/cheggaaa/pb/v3"
	"github.com/ktr0731/go-fuzzyfinder"
	"github.com/manifoldco/promptui"
)

const baseSiteURL string = "https://animefire.plus/"

type Episode struct {
	Number string
	Num    int
	URL    string
}

type Anime struct {
	Name     string
	URL      string
	Episodes []Episode
}

type VideoResponse struct {
	Data []VideoData `json:"data"`
}

type VideoData struct {
	Src   string `json:"src"`
	Label string `json:"label"`
}

func isValidURL(url string) bool {
	// Parse the URL to check for validity and to extract the hostname
	parsedURL, err := neturl.Parse(url)
	if err != nil {
		return false
	}

	// Check if the hostname is an IP address
	ip := net.ParseIP(parsedURL.Hostname())
	if ip != nil {
		// If it's an IP address, check if it's disallowed
		return !IsDisallowedIP(ip.String())
	}

	// If the hostname is not an IP address, it's considered valid for this example
	// You might want to add additional checks here depending on your requirements
	return true
}

// func databaseFormatter is unused (U1000)
// Remove the unused function databaseFormatter

func DownloadFolderFormatter(str string) string {
	regex := regexp.MustCompile(`https:\/\/animefire\.plus\/video\/([^\/?]+)`)
	match := regex.FindStringSubmatch(str)
	if len(match) > 1 {
		finalStep := match[1]
		return finalStep
	}
	return ""
}

func IsDisallowedIP(hostIP string) bool {
	ip := net.ParseIP(hostIP)
	return ip.IsMulticast() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate()
}

func SafeTransport(timeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := net.DialTimeout(network, addr, timeout)
			if err != nil {
				return nil, err
			}
			ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
			if IsDisallowedIP(ip) {
				return nil, errors.New("ip address is not allowed")
			}
			return c, err
		},
		DialTLS: func(network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: timeout}
			c, err := tls.DialWithDialer(dialer, network, addr, &tls.Config{
				MinVersion: tls.VersionTLS12, // Set minimum TLS version to 1.3 or 1.2 in case break download
			})
			if err != nil {
				return nil, err
			}

			ip, _, _ := net.SplitHostPort(c.RemoteAddr().String())
			if IsDisallowedIP(ip) {
				return nil, errors.New("ip address is not allowed")
			}

			err = c.Handshake()
			if err != nil {
				return c, err
			}

			return c, c.Handshake()
		},
		TLSHandshakeTimeout: timeout,
	}
}

func SafeGet(url string) (*http.Response, error) {
	const clientConnectTimeout = time.Second * 10
	httpClient := &http.Client{
		Transport: SafeTransport(clientConnectTimeout),
	}
	return httpClient.Get(url)
}

func isSeries(animeURL string) (bool, int, error) {
	episodes, err := getAnimeEpisodes(animeURL)
	if err != nil {
		return false, 0, err
	}

	// Retorna true se o número de episódios for maior que 1, indicando uma série
	return len(episodes) > 1, len(episodes), nil
}


func extractVideoURL(url string) (string, error) {
	response, err := SafeGet(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch URL: %v", err)
	}
	defer response.Body.Close()

	doc, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		return "", fmt.Errorf("failed to parse HTML: %v", err)
	}

	videoElements := doc.Find("video")
	if videoElements.Length() == 0 {
		videoElements = doc.Find("div")
	}

	if videoElements.Length() == 0 {
		return "", errors.New("no video elements found in the HTML")
	}

	videoSrc, _ := videoElements.Attr("data-video-src")
	return videoSrc, nil
}

func extractActualVideoURL(videoSrc string) (string, error) {
	response, err := SafeGet(videoSrc)
	if err != nil {
		return "", fmt.Errorf("failed to fetch video source: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request failed with status: %s", response.Status)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %v", err)
	}

	var videoResponse VideoResponse
	if err := json.Unmarshal(body, &videoResponse); err != nil {
		return "", fmt.Errorf("failed to unmarshal JSON response: %v", err)
	}

	if len(videoResponse.Data) == 0 {
		return "", errors.New("no video data found in the response")
	}

	// Function to compare video quality labels and return the highest quality video URL
	highestQualityVideoURL := selectHighestQualityVideo(videoResponse.Data)
	if highestQualityVideoURL == "" {
		return "", errors.New("no suitable video quality found")
	}

	return highestQualityVideoURL, nil
}

// Assumes that the quality label contains resolution information (e.g., "1080p").
// This function can be adapted based on the actual format of the quality labels.
func selectHighestQualityVideo(videos []VideoData) string {
	var highestQuality string
	var highestQualityURL string
	for _, video := range videos {
		if isHigherQuality(video.Label, highestQuality) {
			highestQuality = video.Label
			highestQualityURL = video.Src
		}
	}
	return highestQualityURL
}

// Compares two quality labels and returns true if the first is of higher quality than the second.
func isHigherQuality(quality1, quality2 string) bool {
	// Extract numeric part of the quality labels (assuming format "720p", "1080p", etc.)
	quality1Value, _ := strconv.Atoi(strings.TrimRight(quality1, "p"))
	quality2Value, _ := strconv.Atoi(strings.TrimRight(quality2, "p"))
	return quality1Value > quality2Value
}

func PlayVideo(videoURL string, episodes []Episode, currentEpisodeNum int) error {
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		cmd := exec.Command("vlc", "-vvv", videoURL)
		if err := cmd.Start(); err != nil {
			fmt.Printf("Failed to start video player: %v\n", err)
			return
		}

		if err := cmd.Wait(); err != nil {
			fmt.Printf("Failed to play video: %v\n", err)
		}
	}()

	// Find the index of the current episode based on Num
	currentEpisodeIndex := -1
	for i, ep := range episodes {
		if ep.Num == currentEpisodeNum {
			currentEpisodeIndex = i
			break
		}
	}

	// If the current episode was not found, return an error or handle appropriately
	if currentEpisodeIndex == -1 {
		log.Printf("Current episode number %d not found", currentEpisodeNum)
		return errors.New("current episode not found")
	}

	// Command listener for navigating episodes
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("Press 'n' for next episode, 'p' for previous episode, 'q' to quit:")

	for {
		char, _, err := reader.ReadRune()
		if err != nil {
			fmt.Printf("Failed to read command: %v\n", err)
			break
		}

		switch char {
		case 'n':
			if currentEpisodeIndex+1 < len(episodes) {
				nextEpisode := episodes[currentEpisodeIndex+1]
				fmt.Printf("Switching to next episode: %s\n", nextEpisode.Number)
				wg.Wait() // Wait for the current video to stop
				nextVideoURL, err := getVideoURLForEpisode(nextEpisode.URL)
				if err != nil {
					fmt.Printf("Failed to get video URL for next episode: %v\n", err)
					continue
				}
				return PlayVideo(nextVideoURL, episodes, nextEpisode.Num)
			} else {
				fmt.Println("Already at the last episode.")
			}
		case 'p':
			if currentEpisodeIndex > 0 {
				prevEpisode := episodes[currentEpisodeIndex-1]
				fmt.Printf("Switching to previous episode: %s\n", prevEpisode.Number)
				wg.Wait() // Wait for the current video to stop
				prevVideoURL, err := getVideoURLForEpisode(prevEpisode.URL)
				if err != nil {
					fmt.Printf("Failed to get video URL for previous episode: %v\n", err)
					continue
				}
				return PlayVideo(prevVideoURL, episodes, prevEpisode.Num)
			} else {
				fmt.Println("Already at the first episode.")
			}
		case 'q':
			fmt.Println("Quitting video playback.")
			return nil
		}
	}

	wg.Wait()
	return nil
}



func getVideoURLForEpisode(episodeURL string) (string, error) {
	// Assuming extractVideoURL and extractActualVideoURL functions are defined elsewhere
	videoURL, err := extractVideoURL(episodeURL)
	if err != nil {
		return "", err
	}
	return extractActualVideoURL(videoURL)
}

func selectAnimeWithGoFuzzyFinder(animes []Anime) (string, error) {
	if len(animes) == 0 {
		return "", errors.New("no anime provided")
	}

	animeNames := make([]string, len(animes))
	for i, anime := range animes {
		animeNames[i] = anime.Name
	}

	idx, err := fuzzyfinder.Find(
		animeNames,
		func(i int) string {
			return animeNames[i]
		},
	)
	if err != nil {
		return "", fmt.Errorf("failed to select anime with go-fuzzyfinder: %v", err)
	}

	if idx < 0 || idx >= len(animes) {
		return "", errors.New("invalid index returned by fuzzyfinder")
	}

	return animes[idx].Name, nil
}

func DownloadVideo(url string, destPath string, numThreads int) error {
	// Certifique-se de que o caminho de destino é validado para evitar a travessia de diretório
	destPath = filepath.Clean(destPath)

	// Crie um cliente HTTP seguro usando SafeTransport
	const clientConnectTimeout = 10 * time.Second
	httpClient := &http.Client{
		Transport: SafeTransport(clientConnectTimeout),
	}

	// Faça uma solicitação HEAD para obter o tamanho do conteúdo
	resp, err := httpClient.Head(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Verifique se o servidor suporta conteúdo parcial
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("o servidor não suporta conteúdo parcial: código de status %d", resp.StatusCode)
	}

	// Obtenha o tamanho do conteúdo
	contentLength, err := strconv.Atoi(resp.Header.Get("Content-Length"))
	if err != nil {
		return err
	}

	// Calcule o tamanho de cada parte
	chunkSize := contentLength / numThreads
	var wg sync.WaitGroup

	// Crie barras de progresso para cada parte
	bars := make([]*pb.ProgressBar, numThreads)
	for i := range bars {
		bars[i] = pb.Full.Start64(int64(chunkSize))
	}
	pool, err := pb.StartPool(bars...)
	if err != nil {
		return err
	}

	// Faça o download de cada parte em uma goroutine separada
	for i := 0; i < numThreads; i++ {
		from := i * chunkSize
		to := from + chunkSize - 1
		if i == numThreads-1 {
			to = contentLength - 1
		}

		wg.Add(1)
		go func(from, to, part int, bar *pb.ProgressBar) {
			defer wg.Done()

			if !isValidURL(url) {
				log.Printf("Thread %d: unsafe URL detected, aborting request\n", part)
				return
			}

			// Crie uma solicitação GET com um cabeçalho Range
			req, err := http.NewRequest("GET", url, nil)
			if err != nil {
				log.Printf("Thread %d: erro ao criar a solicitação: %v\n", part, err)
				return
			}
			rangeHeader := fmt.Sprintf("bytes=%d-%d", from, to)
			req.Header.Add("Range", rangeHeader)

			// Faça a solicitação usando o cliente HTTP seguro
			resp, err := httpClient.Do(req)
			if err != nil {
				log.Printf("Thread %d: erro na solicitação: %v\n", part, err)
				return
			}
			defer resp.Body.Close()

			// Crie um arquivo para a parte baixada
			partFileName := fmt.Sprintf("%s.part%d", filepath.Base(destPath), part)
			partFilePath := filepath.Join(filepath.Dir(destPath), partFileName)
			file, err := os.Create(partFilePath)
			if err != nil {
				log.Printf("Thread %d: erro ao criar o arquivo: %v\n", part, err)
				return
			}

			defer file.Close()

			// Escreva os dados no arquivo e atualize a barra de progresso
			buf := make([]byte, 1024)
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					_, writeErr := file.Write(buf[:n])
					if writeErr != nil {
						log.Printf("Thread %d: erro ao escrever no arquivo: %v\n", part, writeErr)
						return
					}
					bar.Add(n)
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					log.Printf("Thread %d: erro ao ler o corpo da resposta: %v\n", part, err)
					return
				}
			}
			bar.Finish()
		}(from, to, i, bars[i])
	}

	// Aguarde todas as goroutines terminarem
	wg.Wait()
	pool.Stop()

	// Combine todas as partes em um único arquivo
	outFile, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	for i := 0; i < numThreads; i++ {
		partFileName := fmt.Sprintf("%s.part%d", filepath.Base(destPath), i)
		partFilePath := filepath.Join(filepath.Dir(destPath), partFileName)

		partFile, err := os.Open(partFilePath)
		if err != nil {
			return err
		}

		_, err = io.Copy(outFile, partFile)
		partFile.Close()
		os.Remove(partFilePath)

		if err != nil {
			return err
		}
	}

	return nil
}

func searchAnime(animeName string) (string, error) {
	currentPageURL := fmt.Sprintf("%s/pesquisar/%s", baseSiteURL, animeName)

	for {
		response, err := http.Get(currentPageURL)
		if err != nil {
			return "", fmt.Errorf("failed to perform search request: %v", err)
		}
		defer response.Body.Close()

		doc, err := goquery.NewDocumentFromReader(response.Body)
		if err != nil {
			return "", fmt.Errorf("failed to parse response: %v", err)
		}

		animes := make([]Anime, 0)
		doc.Find(".row.ml-1.mr-1 a").Each(func(i int, s *goquery.Selection) {
			anime := Anime{
				Name: strings.TrimSpace(s.Text()),
				URL:  s.AttrOr("href", ""),
			}
			animeName = strings.TrimSpace(s.Text())

			animes = append(animes, anime)
		})

		if len(animes) > 0 {
			selectedAnimeName, _ := selectAnimeWithGoFuzzyFinder(animes)
			for _, anime := range animes {
				if anime.Name == selectedAnimeName {
					return anime.URL, nil
				}
			}
		}

		nextPage, exists := doc.Find(".pagination .next a").Attr("href")
		if !exists || nextPage == "" {
			return "", fmt.Errorf("no anime found with the given name")
		}

		currentPageURL = baseSiteURL + nextPage
	}
}

func treatingAnimeName(animeName string) string {
	loweredName := strings.ToLower(animeName)
	return strings.ReplaceAll(loweredName, " ", "-")
}

func getUserInput(label string) string {
	prompt := promptui.Prompt{
		Label: label,
	}

	result, err := prompt.Run()
	if err != nil {
		log.Fatalf("Error acquiring user input: %v", err)
	}
	return result
}

func askForDownload() bool {
	prompt := promptui.Select{
		Label: "Do you want to download the episode",
		Items: []string{"Yes", "No"},
	}

	_, result, err := prompt.Run()
	if err != nil {
		log.Fatalf("Error acquiring user input: %v", err)
	}
	return strings.ToLower(result) == "yes"
}

func askForPlayOffline() bool {
	prompt := promptui.Select{
		Label: "Do you want to play the downloaded version offline",
		Items: []string{"Yes", "No"},
	}

	_, result, err := prompt.Run()
	if err != nil {
		log.Fatalf("Error acquiring user input: %v", err)
	}
	return strings.ToLower(result) == "yes"
}

func getAnimeEpisodes(animeURL string) ([]Episode, error) {
	resp, err := SafeGet(animeURL)
	if err != nil {
		return nil, fmt.Errorf("failed to get anime details: %v", err)
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse anime details: %v", err)
	}

	episodeContainer := doc.Find("a.lEp.epT.divNumEp.smallbox.px-2.mx-1.text-left.d-flex")

	var episodes []Episode
	episodeContainer.Each(func(i int, s *goquery.Selection) {
		episodeNum := s.Text()
		episodeURL, _ := s.Attr("href")

		// Parse episode number from episodeNum string
		numRe := regexp.MustCompile(`\d+`)
		numStr := numRe.FindString(episodeNum)
		num, err := strconv.Atoi(numStr)
		if err != nil {
			log.Printf("Error parsing episode number '%s': %v", episodeNum, err)
			return
		}

		episode := Episode{
			Number: episodeNum,
			Num:    num,
			URL:    episodeURL,
		}
		episodes = append(episodes, episode)
	})

	// Sort episodes by Num
	sort.Slice(episodes, func(i, j int) bool {
		return episodes[i].Num < episodes[j].Num
	})

	return episodes, nil
}

func selectEpisode(episodes []Episode) (string, string) {
	templates := &promptui.SelectTemplates{
		Label:    "{{ . }}",
		Active:   "▶ {{ .Number | cyan }}",
		Inactive: "  {{ .Number | white }}",
		Selected: "▶ {{ .Number | cyan | underline }}",
	}

	prompt := promptui.Select{
		Label:     "Select the episode",
		Items:     episodes,
		Templates: templates,
	}

	index, _, err := prompt.Run()
	if err != nil {
		log.Fatalf("Failed to select episode: %v", err)
	}
	return episodes[index].URL, episodes[index].Number
}

func handleDownloadAndPlay(videoURL string, episodes []Episode, selectedEpisodeNum int, animeURL, episodeNumberStr string) {
	if askForDownload() {
		currentUser, err := user.Current()
		if err != nil {
			log.Fatalf("Failed to get current user: %v", err)
		}

		downloadPath := filepath.Join(currentUser.HomeDir, ".local", "goanime", "downloads", "anime", DownloadFolderFormatter(animeURL))
		episodePath := filepath.Join(downloadPath, episodeNumberStr+".mp4")

		if _, err := os.Stat(downloadPath); os.IsNotExist(err) {
			if err := os.MkdirAll(downloadPath, os.ModePerm); err != nil {
				log.Fatalf("Failed to create download directory: %v", err)
			}
		}

		if _, err := os.Stat(episodePath); os.IsNotExist(err) {
			fmt.Println("Downloading the video...")
			numThreads := 4 // Define the number of threads for downloading
			if err := DownloadVideo(videoURL, episodePath, numThreads); err != nil {
				log.Fatalf("Failed to download video: %v", err)
			}
			fmt.Println("Video downloaded successfully!")
		} else {
			fmt.Println("Video already downloaded.")
		}

		if askForPlayOffline() {
			if err := PlayVideo(episodePath, episodes, selectedEpisodeNum); err != nil {
				log.Fatalf("Failed to play video: %v", err)
			}
		}
	} else {
		if err := PlayVideo(videoURL, episodes, selectedEpisodeNum); err != nil {
			log.Fatalf("Failed to play video: %v", err)
		}
	}
}


func main() {
	animeName := getUserInput("Enter anime name")
	animeURL, err := searchAnime(treatingAnimeName(animeName))
	if err != nil {
		log.Fatalf("Failed to get anime episodes: %v", err)
		os.Exit(1)
	}

	episodes, err := getAnimeEpisodes(animeURL)
	if err != nil || len(episodes) == 0 {
		log.Fatalln("Failed to fetch episodes from selected anime")
		os.Exit(1)
	}

	// Check if the anime is a series or a movie/OVA
	series, totalEpisodes, err := isSeries(animeURL)
	if err != nil {
		log.Fatalf("Erro ao verificar se o anime é uma série: %v", err)
	}

	if series {
		fmt.Printf("O anime selecionado é uma série com %d episódios.\n", totalEpisodes)

		selectedEpisodeURL, episodeNumberStr := selectEpisode(episodes)
		// Parse the selected episode's number
		numRe := regexp.MustCompile(`\d+`)
		numStr := numRe.FindString(episodeNumberStr)
		selectedEpisodeNum, err := strconv.Atoi(numStr)
		if err != nil {
			log.Fatalf("Failed to parse selected episode number '%s': %v", episodeNumberStr, err)
		}

		videoURL, err := extractVideoURL(selectedEpisodeURL)
		if err != nil {
			log.Fatalf("Failed to extract video URL: %v", err)
		}

		videoURL, err = extractActualVideoURL(videoURL)
		if err != nil {
			log.Fatalf("Failed to extract the actual video URL: %v", err)
		}

		handleDownloadAndPlay(videoURL, episodes, selectedEpisodeNum, animeURL, episodeNumberStr)
	} else {
		fmt.Println("O anime selecionado é um filme/OVA. Iniciando a reprodução direta...")
		handleDownloadAndPlay(episodes[0].URL, episodes, episodes[0].Num, animeURL, "1")
	}
}

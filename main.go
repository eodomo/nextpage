package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
)

type OllamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type OllamaOptions struct {
	Temperature float64 `json:"temperature"`
	Num_ctx     int64   `json:"num_ctx"`
	Num_predict int64   `json:"num_predict"`
	Seed        int64   `json:"seed"`
}

type OllamaRequestBody struct {
	Model    string          `json:"model"`
	Messages []OllamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Think    bool            `json:"think"`
	Options  OllamaOptions   `json:"options"`
}

type OllamaChatChunk struct {
	Message OllamaMessage `json:"message"`
	Done    bool          `json:"done"`
	Error   string        `json:"string"`
}

func main() {
	log.Println("Building request...")
	address := os.Getenv("OLLAMASERVER")
	contentType := "application/json"
	//query := "Hello! I'm testing out Ollama. Please list out the first 10 digits of the fibonnaci sequcence, and then tell me a joke."
	query := "Explain how a reverse proxy handles an HTTPS request. Describe each step in order and include one example. Write at least 150 words."

	messages := []OllamaMessage{
		{Role: "user", Content: query},
	}
	body := OllamaRequestBody{
		Model:    "qwen2.5:1.5b",
		Messages: messages,
		Stream:   false,
		Think:    false,
		Options: OllamaOptions{
			Temperature: 0,
			Num_ctx:     2048,
			Num_predict: 200,
			Seed:        42,
		},
	}
	bodyJson, err := json.Marshal(body)
	if err != nil {
		log.Fatal(err.Error())
	}

	log.Println("Sending request...")
	req, err := http.NewRequest(http.MethodPost, address, bytes.NewReader(bodyJson))
	if err != nil {
		log.Fatal(err.Error())
	}
	req.Header.Set("Content-Type", contentType)
	req.SetBasicAuth(os.Getenv("USER"), os.Getenv("PASS"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Fatal(resp.Status)
	}
	respBytes, err := io.ReadAll(resp.Body)
	var result map[string]any
	if err := json.Unmarshal(respBytes, &result); err != nil {
		log.Fatal(err.Error())
	}
	log.Println(result)
}

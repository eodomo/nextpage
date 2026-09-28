package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/ollama/ollama/api"
)

type basicAuthTransport struct {
	username string
	password string
	base     http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.SetBasicAuth(t.username, t.password)
	return t.base.RoundTrip(req)
}
func main() {
	log.Println("Building request...")
	server := os.Getenv("OLLAMASERVER")
	model := os.Getenv("MODEL")
	username := os.Getenv("USER")
	password := os.Getenv("PASS")
	if server == "" || username == "" || password == "" {
		log.Fatal("set OLLAMASERVER, MODEL, USER, and PASS")
	}
	baseUrl, err := url.Parse(server)
	if err != nil {
		log.Fatal(err)
	}

	httpClient := &http.Client{
		Transport: &basicAuthTransport{
			username: username,
			password: password,
			base:     http.DefaultTransport,
		},
	}
	client := api.NewClient(baseUrl, httpClient)

	ctx := context.Background()
	stream := true
	log.Println("Sending request...")
	err = client.Generate(
		ctx,
		&api.GenerateRequest{
			Model:  model,
			Prompt: "Explain how a reverse proxy handles an HTTPS request. Describe each step in order and include one example. Write at least 150 words.",
			Stream: &stream,
		},
		func(resp api.GenerateResponse) error {
			fmt.Print(resp.Response)
			return nil
		},
	)
	if err != nil {
		log.Fatal(err)
	}
}

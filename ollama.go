package main

import (
	"context"
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

func buildHttpClient() *http.Client {
	log.Println("Building request...")
	username := os.Getenv("OLLAMAUSER")
	password := os.Getenv("OLLAMAPASS")
	if username == "" || password == "" {
		log.Fatal("set OLLAMAUSER and OLLAMAPASS")
	}

	httpClient := &http.Client{
		Transport: &basicAuthTransport{
			username: username,
			password: password,
			base:     http.DefaultTransport,
		},
	}

	return httpClient
}

func SendRequest(prompt string, model string, onChunk func(string) error) error {
	log.Println("Building request...")
	server := os.Getenv("OLLAMASERVER")
	if server == "" {
		log.Fatal("set OLLAMASERVER")
	}
	baseUrl, err := url.Parse(server)
	if err != nil {
		log.Fatal(err)
	}

	httpClient := buildHttpClient()
	client := api.NewClient(baseUrl, httpClient)

	ctx := context.Background()
	stream := true
	log.Println("Sending request...")
	err = client.Generate(
		ctx,
		&api.GenerateRequest{
			Model:  model,
			Prompt: prompt,
			Stream: &stream,
		},
		func(resp api.GenerateResponse) error {
			if resp.Response == "" {
				return nil
			}
			return onChunk(resp.Response)
		},
	)
	if err != nil {
		return err
	}
	return nil
}

package services

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Request is a generic struct that contains information about how to make a network request
type Request struct {
	method          string
	url             string
	headers         map[string]string
	queryParameters map[string][]string
}

// RequestError is returned when the server responds with a non-200 status,
// so callers can react to specific codes (e.g. 401 → re-authenticate).
type RequestError struct {
	StatusCode int
	Body       string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("request failed with status %d: %s", e.StatusCode, e.Body)
}

var client = http.Client{Timeout: 10 * time.Second}

// MakeRequest makes a network request and unmarshalls the data
func MakeRequest(request Request, responseObject interface{}) error {
	req, err := http.NewRequest(request.method, request.url, nil)
	if err != nil {
		return err
	}
	for key, value := range request.headers {
		req.Header.Set(key, value)
	}

	queryParams := req.URL.Query()
	for key, value := range request.queryParameters {
		for _, queryValue := range value {
			queryParams.Add(key, queryValue)
		}
	}
	req.URL.RawQuery = queryParams.Encode()

	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("error reading response: %w", err)
	}

	if res.StatusCode != http.StatusOK {
		return &RequestError{StatusCode: res.StatusCode, Body: string(body)}
	}

	if err := json.Unmarshal(body, &responseObject); err != nil {
		return fmt.Errorf("error parsing JSON from network request: %w", err)
	}
	return nil
}

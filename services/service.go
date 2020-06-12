package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
)

// Request is a generic struct that contains information about how to make a network request
type Request struct {
	method          string
	url             string
	headers         map[string]string
	queryParameters map[string][]string
}

var client = http.Client{}

// MakeRequest makes a network request and unmarshalls the data
func MakeRequest(request Request, responseObject interface{}) error {

	req, _ := http.NewRequest(request.method, request.url, nil)
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

	res, error := client.Do(req)
	if error != nil {
		fmt.Println(error)
		return error
	}
	defer res.Body.Close()

	body, error := ioutil.ReadAll(res.Body)
	if error != nil {
		return errors.New("Error reading response")
	}

	if res.StatusCode != 200 {
		return errors.New("Error making request, got status code " + strconv.Itoa(res.StatusCode) + " " + string(body))
	}

	err := json.Unmarshal(body, &responseObject)
	if err != nil {
		log.Println("Error parsing JSON from network request: ", err)
		return errors.New("Error parsing JSON")
	}
	return nil
}

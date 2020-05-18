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

type Request struct {
	method          string
	url             string
	headers         map[string]string
	queryParameters map[string][]string
}

var client = http.Client{}

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
	defer res.Body.Close()

	if error != nil {
		fmt.Println(error)
		return error
	}

	body, error := ioutil.ReadAll(res.Body)
	if error != nil {
		return errors.New("Error reading response")
	}

	if res.StatusCode != 200 {
		return errors.New("Error making request, got status code " + strconv.Itoa(res.StatusCode) + " " + string(body))
	}

	err := json.Unmarshal(body, &responseObject)
	if err != nil {
		log.Fatalln(err)
		return errors.New("Error parsing JSON")
	}
	return nil
}

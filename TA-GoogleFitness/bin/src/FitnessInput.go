package main

import (
	"bufio"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/AndyNortrup/GoSplunk"
)

const APP_NAME string = "TA-GoogleFitness"
const STRATEGY_GOOGLE string = "GoogleFitness"
const STRATEGY_FITBIT string = "FitBit"
const STRATEGY_MICROSOFT string = "Microsoft"
const STRATEGY_PARAM_NAME string = "FitnessService"
const ENFORCE_CERT_VALIDATION string = "force_cert_validation"

type FitnessInput struct {
	*splunk.ModInputConfig
	reader io.Reader //Location to read configurations from
	writer io.Writer //Location to write configurations to
}

//Write the scheme to input.writer
func (input *FitnessInput) ReturnScheme() {
	arguments := append([]splunk.Argument{}, splunk.Argument{
		Name:        ENFORCE_CERT_VALIDATION,
		Title:       "ForceCertValidation",
		Description: "If true the input requires certificate validation when making REST calls to Splunk",
		DataType:    "boolean",
	},
		splunk.Argument{
			Name:        STRATEGY_PARAM_NAME,
			Title:       "FitnessService",
			Description: "Enter the name of the Fitness Service to be polled.  Options are: 'GoogleFitness', 'FitBit', 'Microsoft'",
			DataType:    "string",
		})

	scheme := &splunk.Scheme{
		Title:                 "Google Fitness",
		Description:           "Retrieves fitness data from Google Fitness.",
		UseExternalValidation: true,
		StreamingMode:         "simple",
		Args:                  arguments,
	}

	enc := xml.NewEncoder(input.writer)
	enc.Indent("   ", "   ")
	if err := enc.Encode(scheme); err != nil {
		fmt.Printf("error: %v\n", err)
	}
}

func (input *FitnessInput) ValidateScheme() (bool, string) {
	config, err := splunk.ReadModInputConfig(input.reader)
	if err != nil {
		return false, "Unable to parse configuration." + err.Error()
	}

	for _, stanza := range config.Stanzas {
		for _, param := range stanza.Params {
			//Check that the parameter STRAGEGY_PARAM_NAME is one of our defined
			// strategies for getting data
			if param.Name == STRATEGY_PARAM_NAME &&
				!(param.Value == STRATEGY_GOOGLE ||
					param.Value == STRATEGY_FITBIT ||
					param.Value == STRATEGY_MICROSOFT) {
				return false, "Improper service '" + param.Value + "' name indicated."
			}
		}
	}
	return true, ""
}

func (input *FitnessInput) StreamEvents() {

	config, err := splunk.ReadModInputConfig(input.reader)
	if err != nil {
		log.Printf("Unable to read Modular Input config from reader.")
	}
	input.ModInputConfig = config

	//TODO: Replace hard coded values with pull from storage/passwords
	// tok := newToken("1/7u5ngLKEF2MiVYHvnWwYKRIb8s3s8u2e8JtHZ2yjUAQ",
	// 	"ya29.Ci8IA_du7mknNus-G_UTfiWB3FHeqdpIqEj_bwaUSvB2lYvsZSuKB7E-2TVuDM44sw",
	// 	"2016-06-21 07:59:23.44961918 -0700 PDT",
	// 	"Bearer")

	tokens := input.getTokens()

	for _, token := range tokens {
		//Create HTTP client
		clientId, clientSecret := input.getAppCredentials()
		client := getClient(token, clientId, clientSecret)

		//Get start and end points from checkpoint
		startTime, endTime := input.getTimes()

		//Create a Fitness Reader to go get the data
		fitnessReader, err := input.getReaderStrategy(startTime, endTime)
		if err != nil {
			log.Fatal(err)
		}

		input.writeCheckPoint(fitnessReader.getData(client, bufio.NewWriter(os.Stdout)))
	}
}

func (input *FitnessInput) getReaderStrategy(startTime time.Time, endTime time.Time) (FitnessReader, error) {
	strategy := input.getStrategy()
	switch {
	case strategy == STRATEGY_GOOGLE:
		reader := &GoogleFitnessReader{startTime: startTime, endTime: endTime}
		return reader, nil
	default:
		return nil, errors.New("Unsupported reader requested: " + strategy)
	}
}

//get the value of the strategy parameter from the configuration.
func (input *FitnessInput) getStrategy() string {
	var strategy string

	for _, stanza := range input.Stanzas {
		for _, param := range stanza.Params {
			if param.Name == STRATEGY_PARAM_NAME {
				strategy = param.Value
			}
		}
	}
	if strategy == "" {
		log.Fatalf("No strategy passed to Fitness Input")
	}
	return strategy
}

// getAppCredentials makes a call to the storage/passwords enpoint and retrieves
// an appId and clinetSecret for the application.  The appId is stored in the
// password field of the endpoint data and the appId is in the username.
func (input *FitnessInput) getAppCredentials() (string, string) {
	passwords, err := splunk.GetEntities(splunk.LocalSplunkMgmntURL,
		[]string{"storage", "passwords"},
		APP_NAME,
		"nobody",
		input.SessionKey)

	if err != nil {
		log.Fatalf("Unable to retrieve password entries for TA-GoogleFitness: %v\n",
			err)
	}

	var clientSecret string
	var clientId string

	for _, entry := range passwords.Entries {
		//Because there could/should be multiple stored passwords we need to check
		// the id for `apps.googleusercontent.com` because the id is based on the
		// username.

		if strings.Contains(entry.ID, "apps.googleusercontent.com") {
			for _, key := range entry.Contents.Keys {
				if key.Name == "clear_password" {
					clientSecret = key.Value
				}
				if key.Name == "username" {
					clientId = key.Value
				}
			}
		}
	}
	return clientId, clientSecret
}

// getTokens gets a list of tokens that are in the storage/passwords endpoint
// for the given strategy
func (input *FitnessInput) getTokens() []*oauth2.Token {
	entities, err := splunk.GetEntities(splunk.LocalSplunkMgmntURL,
		[]string{"storage", "passwords"},
		APP_NAME,
		"nobody",
		input.SessionKey)

	if err != nil {
		log.Fatalf("Unable to get user tokens from Splunk: %v\n", err)
	}

	var result []*oauth2.Token

	for _, entry := range entities.Entries {
		isForStrategy := false
		var tokenJSON string

		// Itterate through all of the password entries
		for _, key := range entry.Contents.Keys {
			switch {
			//Grab the plaintext password
			case key.Name == "clear_password":
				tokenJSON = key.Value
			//Determine if this key matches our strategy
			case key.Name == "realm" && key.Value == input.getStrategy():
				isForStrategy = true

			}
		}

		if isForStrategy {

			//Temporary struct so we can get string values out then make a JSON token
			// by properly converting the date stamp
			type tokenData struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				TokenType    string `json:"token_type"`
				Expires      string `json:"expires_at"`
			}
			temp := &tokenData{}
			decode := json.NewDecoder(strings.NewReader(tokenJSON))
			err := decode.Decode(temp)
			if err != nil {
				log.Fatalf("Failed to decode passwords from storage/passwords: %v\n JSON to Decode: %v\n", err, tokenJSON)
			}
			result = append(result, newToken(temp.RefreshToken, temp.AccessToken, temp.Expires, temp.TokenType))
		}
	}
	return result
}

//getTimes returns a startTime and an endTime value.  endTime is retrived from
// a checkpoint file, if not it returns the current time.
// The end time is always the current time.
func (input *FitnessInput) getTimes() (time.Time, time.Time) {
	startTime, err := input.readCheckPoint()
	if err != nil {
		startTime = time.Now()
	}
	endTime := time.Now()
	return startTime, endTime
}

func (input *FitnessInput) writeCheckPoint(t time.Time) {

	//Encode the time we've been given into bytes
	g, err := t.GobEncode()
	if err != nil {
		log.Fatalf("Unable to encode checkpoint time: %v\n", err)
	}

	//Write the checkpoint
	err = ioutil.WriteFile(input.getCheckPointPath(), g, 0644)
	if err != nil {
		log.Fatalf("Error writing checkpoint file: %v\n", err)
	}
}

func (input *FitnessInput) readCheckPoint() (time.Time, error) {
	b, err := ioutil.ReadFile(input.getCheckPointPath())
	if err != nil {
		log.Printf("Unable to read checkpoint file:%v\n", err)
		return time.Now(), err
	}
	var t time.Time
	err = t.GobDecode(b)
	if err != nil {
		log.Printf("Unable to decode checkpoint file: %v\n", err)
		return time.Now().Add(-2 * time.Hour), err
	}
	return t, nil
}

// Takes the checkpoint dir from and config stanza name from the input and
// creates a checkpoint dir.  Should be unique for each input
func (input *FitnessInput) getCheckPointPath() string {
	//Create a hash of the stanza name as a filename
	fileName := strings.Split(input.Stanzas[0].StanzaName, "://")
	path := path.Join(input.CheckpointDir, fileName[1])
	return path
}
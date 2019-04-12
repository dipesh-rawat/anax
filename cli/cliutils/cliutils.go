package cliutils

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	dockerclient "github.com/fsouza/go-dockerclient"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/exchange"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	HZN_API             = "http://localhost"
	HZN_API_MAC         = "http://localhost:8081"
	AGBOT_HZN_API       = "http://localhost:8046"
	JSON_INDENT         = "  "
	MUST_REGISTER_FIRST = "this command can not be run before running 'hzn register'"

	// Exit Codes
	CLI_INPUT_ERROR    = 1 // we actually don't have control over the usage exit code that kingpin returns, so use the same code for input errors we catch ourselves
	JSON_PARSING_ERROR = 3
	FILE_IO_ERROR      = 4
	HTTP_ERROR         = 5
	//EXEC_CMD_ERROR = 6
	CLI_GENERAL_ERROR = 7
	NOT_FOUND         = 8
	SIGNATURE_INVALID = 9
	INTERNAL_ERROR    = 99

	// Anax API HTTP Codes
	ANAX_ALREADY_CONFIGURED = 409
	ANAX_NOT_CONFIGURED_YET = 424

	//anax configuration files
	ANAX_OVERWRITE_FILE = "/etc/default/horizon"
	ANAX_CONFIG_FILE    = "/etc/horizon/anax.json"

	DEFAULT_EXCHANGE_URL = "https://alpha.edge-fabric.com/v1/"

	// default keys will be prepended with $HOME
	DEFAULT_PRIVATE_KEY_FILE = ".hzn/keys/service.private.key"
	DEFAULT_PUBLIC_KEY_FILE  = ".hzn/keys/service.public.pem"
)

// Holds the cmd line flags that were set so other pkgs can access
type GlobalOptions struct {
	Verbose     *bool
	IsDryRun    *bool
	UsingApiKey bool // should go away soon
}

var Opts GlobalOptions

type UserExchangeReq struct {
	Password string `json:"password"`
	Admin    bool   `json:"admin"`
	Email    string `json:"email"`
}

func Verbose(msg string, args ...interface{}) {
	if !*Opts.Verbose {
		return
	}
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprintf(os.Stderr, "[verbose] "+msg, args...) // send to stderr so it doesn't mess up stdout if they are piping that to jq or something like that
}

func Fatal(exitCode int, msg string, args ...interface{}) {
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprintf(os.Stderr, "Error: "+msg, args...)
	os.Exit(exitCode)
}

func Warning(msg string, args ...interface{}) {
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprintf(os.Stderr, "Warning: "+msg, args...)
}

func IsDryRun() bool {
	return *Opts.IsDryRun
}

/*
func GetShortBinaryName() string {
	return path.Base(os.Args[0])
}
*/

// SplitIdToken splits an id:token or user:pw and return the parts.
func SplitIdToken(idToken string) (id, token string) {
	parts := strings.SplitN(idToken, ":", 2)
	id = parts[0] // SplitN will always at least return 1 element
	token = ""
	if len(parts) >= 2 {
		token = parts[1]
	}
	return
}

// Unmarshal simply calls json.Unmarshal and handles any errors
func Unmarshal(data []byte, v interface{}, errMsg string) {
	err := json.Unmarshal(data, v)
	if err != nil {
		Fatal(JSON_PARSING_ERROR, "failed to unmarshal bytes from %s: %v", errMsg, err)
	}
}

// MarshalIndent calls json.MarshalIndent and handles any errors
func MarshalIndent(v interface{}, errMsg string) string {
	jsonBytes, err := json.MarshalIndent(v, "", JSON_INDENT)
	if err != nil {
		Fatal(JSON_PARSING_ERROR, "failed to marshal data type from %s: %v", errMsg, err)
	}
	return string(jsonBytes)
}

// SetWhetherUsingApiKey is a hack because some api keys are global and shouldn't be prepended by the org
// an api key or device id/token.
func SetWhetherUsingApiKey(creds string) {
	if os.Getenv("USING_API_KEY") == "0" {
		return // this is their way of telling us that even though the creds look like an api key it isn't
	}
	// Some API keys start with: a-<6charorgid>-
	if matched, err := regexp.MatchString(`^a-[A-Za-z0-9]{6}-`, creds); err != nil {
		Fatal(INTERNAL_ERROR, "problem testing api key match: %v", err)
	} else if matched {
		Opts.UsingApiKey = true
		Verbose("Using API key")
	}
}

func NewDockerClient() (client *dockerclient.Client) {
	var err error
	dockerEndpoint := "unix:///var/run/docker.sock" // if we need this to be user configurable someday, we can get it from an env var
	if client, err = dockerclient.NewClient(dockerEndpoint); err != nil {
		Fatal(CLI_GENERAL_ERROR, "unable to create docker client: %v", err)
	}
	return
}

// GetDockerAuth finds the docker credentials for this registry in ~/.docker/config.json
func GetDockerAuth(domain string) (auth dockerclient.AuthConfiguration, err error) {
	var auths *dockerclient.AuthConfigurations
	if auths, err = dockerclient.NewAuthConfigurationsFromDockerCfg(); err != nil {
		return
	}

	for domainName, creds := range auths.Configs {
		Verbose("docker auth domainName: %v", domainName)
		if (domainName == domain) || (domain == "" && strings.Contains(domainName, "docker.io/")) {
			auth = creds
			return
		}
	}

	err = errors.New(fmt.Sprintf("unable to find docker credentials for %v", domain))
	return
}

// PushDockerImage pushes the image to its docker registry, outputting progress to stdout. It returns the repo digest. If there is an error, it prints the error and exits.
// We don't have to handle the case of a digest in the image name, because in that case we assume the image has already been pushed (that is the way to get the digest).
func PushDockerImage(client *dockerclient.Client, domain, path, tag string) (digest string) {
	var repository string // for PushImageOptions later on
	if domain == "" {
		repository = path
	} else {
		repository = domain + "/" + path
	}
	fmt.Printf("Pushing %v:%v...\n", repository, tag) // Note: tag can be the empty string

	// Get the docker client object for this registry, and set the push options and creds
	var buf bytes.Buffer
	multiWriter := io.MultiWriter(os.Stdout, &buf)                                               // we want output of the push to go 2 places: stdout (for the user to see progess) and a variable (so we can get the digest value)
	opts := dockerclient.PushImageOptions{Name: repository, Tag: tag, OutputStream: multiWriter} // do not set InactivityTimeout because the user will ctrl-c if they think something is wrong

	var auth dockerclient.AuthConfiguration
	var err error
	if auth, err = GetDockerAuth(domain); err != nil {
		Fatal(CLI_INPUT_ERROR, "could not get docker credentials from ~/.docker/config.json: %v. Maybe you need to run 'docker login ...' to provide credentials for the image registry.", err)
	}

	// Now actually push the image
	if err = client.PushImage(opts, auth); err != nil {
		Fatal(CLI_GENERAL_ERROR, "unable to push docker image %v: %v", repository+":"+tag, err)
	}

	// Get the digest value that docker calculated when pushing the image
	//fmt.Printf("DEBUG: docker push output is: %s\n", buf.String())
	reDigest := regexp.MustCompile(`\s+digest:\s+(\S+)\s+size:`)
	var matches []string
	if matches = reDigest.FindStringSubmatch(buf.String()); len(matches) < 2 {
		Fatal(CLI_GENERAL_ERROR, "could not find the image digest in the docker push output")
	}
	digest = matches[1]
	return
}

// OrgAndCreds prepends the org to creds (separated by /) unless creds already has an org prepended
func OrgAndCreds(org, creds string) string {
	// org is the org of the resource being accessed, so if they want to use creds from a different org, the prepend that org to creds before calling this
	if Opts.UsingApiKey || os.Getenv("USING_API_KEY") == "1" { // leaving this code here, because we might need it for ibm cloud api keys
		return creds
	}
	id, _ := SplitIdToken(creds) // only look for the / in the id, because the token is more likely to have special chars
	if strings.Contains(id, "/") {
		return creds // already has the org at the beginning
	}
	return org + "/" + creds
}

// AddSlash prepends "/" to the id if it is not the empty string and returns it. This is useful when id is the last thing in the route.
func AddSlash(id string) string {
	if id == "" {
		return id
	}
	return "/" + id
}

// TrimOrg returns id with the leading "<org>/" removed, if it was there. This is useful because in list sub-cmds id is shown with
// the org prepended, but when the id is put in routes it can not have the org prepended, because org is already earlier in the route.
func TrimOrg(org, id string) (string, string) {
	substrings := strings.Split(id, "/")
	if len(substrings) <= 1 { // this means id was empty, or did not contain '/'
		return org, id
	} else if len(substrings) == 2 {
		return substrings[0], substrings[1] // in this case the org the prepended to the id will override the org they may have specified thru the -o flag or env var
	} else {
		Fatal(CLI_INPUT_ERROR, "the resource id can not contain more than 1 '/'")
	}
	return "", "" // will never get here
}

// FormExchangeId combines url, version, arch the same way the exchange does to form the resource ID.
func FormExchangeIdForService(url, version, arch string) string {
	// Remove the https:// from the beginning of workloadUrl and replace troublesome chars with a dash.
	//val workloadUrl2 = """^[A-Za-z0-9+.-]*?://""".r replaceFirstIn (url, "")
	//val workloadUrl3 = """[$!*,;/?@&~=%]""".r replaceAllIn (workloadUrl2, "-")     // I think possible chars in valid urls are: $_.+!*,;/?:@&~=%-
	//return OrgAndId(orgid, workloadUrl3 + "_" + version + "_" + arch).toString
	url1 := FormExchangeIdWithSpecRef(url)
	return url1 + "_" + version + "_" + arch
}

// Remove the https:// from the beginning of workloadUrl and replace troublesome chars with a dash.
func FormExchangeIdWithSpecRef(specRef string) string {
	re := regexp.MustCompile(`^[A-Za-z0-9+.-]*?://`)
	specRef2 := re.ReplaceAllLiteralString(specRef, "")
	return FormExchangeId(specRef2)
}

// Replace unwanted charactore with - in the id
func FormExchangeId(id string) string {
	re := regexp.MustCompile(`[$!*,;/?@&~=%]`)
	return re.ReplaceAllLiteralString(id, "-")
}

// ReadStdin reads from stdin, and returns it as a byte array.
func ReadStdin() []byte {
	fileBytes, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		Fatal(FILE_IO_ERROR, "reading stdin failed: %v", err)
	}
	return fileBytes
}

// ReadFile reads from a file or stdin, and returns it as a byte array.
func ReadFile(filePath string) []byte {
	var fileBytes []byte
	var err error
	if filePath == "-" {
		fileBytes, err = ioutil.ReadAll(os.Stdin)
	} else {
		fileBytes, err = ioutil.ReadFile(filePath)
	}
	if err != nil {
		Fatal(FILE_IO_ERROR, "reading %s failed: %v", filePath, err)
	}
	return fileBytes
}

// ExpandMapping is used in ExpandEnv() to print a warning if the env var is not defined.
func ExpandMapping(envVarName string) string {
	envVarValue := os.Getenv(envVarName)
	if envVarValue == "" {
		fmt.Printf("Warning: environment variable '%s' is referenced in input file, but not defined in the environment.\n", envVarName)
	}
	return envVarValue
}

// ExpandEnv is equivalent to os.ExpandEnv(), except prints a warning when an env var is not defined
func ExpandEnv(s string) string {
	return os.Expand(s, ExpandMapping)
}

// ReadJsonFile reads json from a file or stdin, eliminates comments, substitutes env vars, and returns it.
func ReadJsonFile(filePath string) []byte {
	var fileBytes []byte
	var err error
	if filePath == "-" {
		fileBytes, err = ioutil.ReadAll(os.Stdin)
	} else {
		fileBytes, err = ioutil.ReadFile(filePath)
	}
	if err != nil {
		Fatal(FILE_IO_ERROR, "reading %s failed: %v", filePath, err)
	}

	// Remove /* */ comments
	re := regexp.MustCompile(`(?s)/\*.*?\*/`)
	newBytes := re.ReplaceAll(fileBytes, nil)

	// Replace env vars
	if os.Getenv("HZN_DONT_SUBST_ENV_VARS") == "1" {
		return newBytes
	}
	str := ExpandEnv(string(newBytes))
	return []byte(str)
}

// ConfirmRemove prompts the user to confirm they want to run the destructive cmd
func ConfirmRemove(question string) {
	// Prompt the user to make sure he/she wants to do this
	fmt.Print(question + " [y/N]: ")
	var response string
	fmt.Scanln(&response)
	if strings.TrimSpace(response) != "y" {
		fmt.Println("Exiting.")
		os.Exit(0)
	}
}

// WithDefaultEnvVar returns the specified flag ptr if it has a non-blank value, or the env var value.
func WithDefaultEnvVar(flag *string, envVarName string) *string {
	if *flag != "" {
		return flag
	}
	newFlag := os.Getenv(envVarName)
	if newFlag != "" {
		return &newFlag
	}
	return flag // it is empty, but we did not find an env var value
}

// RequiredWithDefaultEnvVar returns the specified flag ptr if it has a non-blank value, or the env var value.
func RequiredWithDefaultEnvVar(flag *string, envVarName, errMsg string) *string {
	if *flag != "" {
		return flag
	}
	newFlag := os.Getenv(envVarName)
	if newFlag != "" {
		return &newFlag
	}
	Fatal(CLI_INPUT_ERROR, errMsg)
	return flag // won't ever happen, here just to make intellij happy
}

// GetHorizonUrlBase returns the base part of the horizon api url (which can be overridden by env var HORIZON_URL)
func GetHorizonUrlBase() string {
	envVar := os.Getenv("HORIZON_URL")
	if envVar != "" {
		return envVar
	}
	if runtime.GOOS == "darwin" {
		return HZN_API_MAC
	} else {
		return HZN_API
	}
}

// GetRespBodyAsString converts an http response body to a string
func GetRespBodyAsString(responseBody io.ReadCloser) string {
	if responseBody == nil {
		return ""
	}

	buf := new(bytes.Buffer)
	buf.ReadFrom(responseBody)
	return buf.String()
}

func isGoodCode(actualHttpCode int, goodHttpCodes []int) bool {
	if len(goodHttpCodes) == 0 {
		return true // passing in an empty list of good codes means anything is ok
	}
	for _, code := range goodHttpCodes {
		if code == actualHttpCode {
			return true
		}
	}
	return false
}

func printHorizonRestError(apiMethod string, err error) {
	msg := ""
	if os.Getenv("HORIZON_URL") == "" {
		msg = fmt.Sprintf("Can't connect to the Horizon REST API to run %s. Run 'systemctl status horizon' to check if the Horizon agent is running. Or set HORIZON_URL to connect to another local port that is connected to a remote Horizon agent via a ssh tunnel. Specific error is: %v", apiMethod, err)
	} else {
		msg = fmt.Sprintf("Can't connect to the Horizon REST API to run %s. Maybe the ssh tunnel associated with that port is down? Or maybe the remote Horizon agent at the other end of that tunnel is down. Specific error is: %v", apiMethod, err)
	}
	Fatal(HTTP_ERROR, msg)
}

// HorizonGet runs a GET on the anax api and fills in the specified structure with the json.
// If the list of goodHttpCodes is not empty and none match the actual http code, it will exit with an error. Otherwise the actual code is returned.
// Only if the actual code matches the 1st element in goodHttpCodes, will it parse the body into the specified structure.
// If quiet if true, then the error will be returned, the function returns back to the caller instead of exiting out.
func HorizonGet(urlSuffix string, goodHttpCodes []int, structure interface{}, quiet bool) (httpCode int, retError error) {
	retError = nil

	url := GetHorizonUrlBase() + "/" + urlSuffix
	apiMsg := http.MethodGet + " " + url
	Verbose(apiMsg)
	resp, err := http.Get(url)
	if err != nil {
		if quiet {
			if os.Getenv("HORIZON_URL") == "" {
				retError = fmt.Errorf("Can't connect to the Horizon REST API to run %s. Run 'systemctl status horizon' to check if the Horizon agent is running. Or set HORIZON_URL to connect to another local port that is connected to a remote Horizon agent via a ssh tunnel. Specific error is: %v", apiMsg, err)
			} else {
				retError = fmt.Errorf("Can't connect to the Horizon REST API to run %s. Maybe the ssh tunnel associated with that port is down? Or maybe the remote Horizon agent at the other end of that tunnel is down. Specific error is: %v", apiMsg, err)
			}
			return
		} else {
			printHorizonRestError(apiMsg, err)
		}
	}
	defer resp.Body.Close()
	httpCode = resp.StatusCode
	Verbose("HTTP code: %d", httpCode)
	if !isGoodCode(httpCode, goodHttpCodes) {
		if quiet {
			retError = fmt.Errorf("Bad HTTP code from %s: %d", apiMsg, httpCode)
			return
		} else {
			Fatal(HTTP_ERROR, "bad HTTP code from %s: %d", apiMsg, httpCode)
		}
	}
	if httpCode == goodHttpCodes[0] {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			if quiet {
				retError = fmt.Errorf("Failed to read body response from %s: %v", apiMsg, err)
				return
			} else {
				Fatal(HTTP_ERROR, "failed to read body response from %s: %v", apiMsg, err)
			}
		}
		switch s := structure.(type) {
		case *string:
			// Just return the unprocessed response body
			*s = string(bodyBytes)
		default:
			// Put the response body in the specified struct
			err = json.Unmarshal(bodyBytes, structure)
			if err != nil {
				if quiet {
					retError = fmt.Errorf("Failed to unmarshal body response from %s: %v", apiMsg, err)
					return
				} else {
					Fatal(JSON_PARSING_ERROR, "failed to unmarshal body response from %s: %v", apiMsg, err)
				}
			}
		}
	}
	return
}

// HorizonDelete runs a DELETE on the anax api.
// If the list of goodHttpCodes is not empty and none match the actual http code, it will exit with an error. Otherwise the actual code is returned.
func HorizonDelete(urlSuffix string, goodHttpCodes []int) (httpCode int) {
	url := GetHorizonUrlBase() + "/" + urlSuffix
	apiMsg := http.MethodDelete + " " + url
	Verbose(apiMsg)
	if IsDryRun() {
		return 204
	}
	httpClient := &http.Client{}
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		Fatal(HTTP_ERROR, "%s new request failed: %v", apiMsg, err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		printHorizonRestError(apiMsg, err)
	}
	defer resp.Body.Close()
	httpCode = resp.StatusCode
	Verbose("HTTP code: %d", httpCode)
	if !isGoodCode(httpCode, goodHttpCodes) {
		Fatal(HTTP_ERROR, "bad HTTP code %d from %s: %s", httpCode, apiMsg, GetRespBodyAsString(resp.Body))
	}
	return
}

// HorizonPutPost runs a PUT or POST to the anax api to create or update a resource.
// If the list of goodHttpCodes is not empty and none match the actual http code, it will exit with an error. Otherwise the actual code is returned.
func HorizonPutPost(method string, urlSuffix string, goodHttpCodes []int, body interface{}) (httpCode int, resp_body string) {
	url := GetHorizonUrlBase() + "/" + urlSuffix
	apiMsg := method + " " + url
	Verbose(apiMsg)
	if IsDryRun() {
		return 201, ""
	}
	httpClient := &http.Client{}

	// Prepare body
	var jsonBytes []byte
	bodyIsBytes := false
	switch b := body.(type) {
	// If the body is a byte array or string, we treat it like a file being uploaded (not multi-part)
	case []byte:
		jsonBytes = b
		bodyIsBytes = true
	case string:
		jsonBytes = []byte(b)
		bodyIsBytes = true
	// Else it is a struct so assume it should be sent as json
	default:
		var err error
		jsonBytes, err = json.Marshal(body)
		if err != nil {
			Fatal(JSON_PARSING_ERROR, "failed to marshal body for %s: %v", apiMsg, err)
		}
	}
	requestBody := bytes.NewBuffer(jsonBytes)

	// Create the request and run it
	req, err := http.NewRequest(method, url, requestBody)
	if err != nil {
		Fatal(HTTP_ERROR, "%s new request failed: %v", apiMsg, err)
	}
	req.Header.Add("Accept", "application/json")
	if bodyIsBytes {
		req.Header.Add("Content-Length", strconv.Itoa(len(jsonBytes)))
	} else {
		req.Header.Add("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		printHorizonRestError(apiMsg, err)
	}

	// Process the response
	defer resp.Body.Close()
	httpCode = resp.StatusCode
	Verbose("HTTP code: %d", httpCode)

	resp_body = GetRespBodyAsString(resp.Body)
	if !isGoodCode(httpCode, goodHttpCodes) {
		Fatal(HTTP_ERROR, "bad HTTP code %d from %s: %s", httpCode, apiMsg, resp_body)
	}
	return
}

// get a value keyed by key in a file. The file contains key=value for each line.
func GetEnvVarFromFile(filename string, key string) (string, error) {
	fHandle, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		} else {
			return "", err
		}
	}
	defer fHandle.Close()

	scanner := bufio.NewScanner(fHandle)
	for scanner.Scan() {
		lineContent := string(scanner.Bytes())
		if strings.Contains(lineContent, key) {
			key_value := strings.Split(lineContent, "=")
			// comment line
			if strings.Contains(key_value[0], "#") {
				continue
			} else if len(key_value) > 1 {
				// trim the leading and trailing space, single quote and double quotes
				s := key_value[1]
				s = strings.TrimSpace(s)
				s = strings.Trim(s, "'")
				s = strings.Trim(s, "\"")
				return s, nil
			} else {
				return "", nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", nil
}

// Get the anax configuration from the given configuration file.
func GetAnaxConfig(configFile string) (*config.HorizonConfig, error) {
	_, err := os.Stat(configFile)
	if os.IsNotExist(err) {
		return nil, nil
	}

	if byteValue, err := ioutil.ReadFile(configFile); err != nil {
		return nil, err
	} else {
		var anaxConfig config.HorizonConfig
		if err := json.Unmarshal(byteValue, &anaxConfig); err != nil {
			return nil, fmt.Errorf("Failed to unmarshal bytes. %v", err)
		} else {
			return &anaxConfig, nil
		}
	}
}

// Get exchange url from /etc/default/horizon file. if not set, check /etc/horizon/anax.json file
func GetExchangeUrlFromAnax() string {
	if value, err := GetEnvVarFromFile(ANAX_OVERWRITE_FILE, "HZN_EXCHANGE_URL"); err != nil {
		Verbose(fmt.Sprintf("Error getting HZN_EXCHANGE_URL from %v. %v", ANAX_OVERWRITE_FILE, err))
	} else if value != "" {
		return value
	}

	if anaxConfig, err := GetAnaxConfig(ANAX_CONFIG_FILE); err != nil {
		Verbose(fmt.Sprintf("Error getting ExchangeUrl from %v. %v", ANAX_CONFIG_FILE, err))
	} else if anaxConfig != nil {
		return anaxConfig.Edge.ExchangeURL
	}

	return ""
}

// GetExchangeUrl returns the exchange url from the env var or anax api
func GetExchangeUrl() string {
	exchUrl := os.Getenv("HZN_EXCHANGE_URL")
	if exchUrl == "" {
		Verbose("HZN_EXCHANGE_URL is not set, get it from horizon agent configuration on the node.")
		value := GetExchangeUrlFromAnax()
		if value != "" {
			exchUrl = value
		} else {
			Verbose("Could not get the exchange url from the horizon agent, using default value: %v", DEFAULT_EXCHANGE_URL)
			exchUrl = DEFAULT_EXCHANGE_URL
		}
	}

	exchUrl = strings.TrimSuffix(exchUrl, "/") // anax puts a trailing slash on it
	if Opts.UsingApiKey || os.Getenv("USING_API_KEY") == "1" {
		re := regexp.MustCompile(`edgenode$`)
		exchUrl = re.ReplaceAllLiteralString(exchUrl, "edge")
	}

	Verbose("The exchange url: %v", exchUrl)
	return exchUrl
}

func printHorizonExchRestError(apiMethod string, err error) {
	if os.Getenv("HZN_EXCHANGE_URL") == "" {
		Fatal(HTTP_ERROR, "Can't connect to the Horizon Exchange REST API to run %s. Set HZN_EXCHANGE_URL to use an Exchange other than the one the Horizon Agent is currently configured for. Specific error is: %v", apiMethod, err)
	} else {
		Fatal(HTTP_ERROR, "Can't connect to the Horizon Exchange REST API to run %s. Maybe HZN_EXCHANGE_URL is set incorrectly? Or unset HZN_EXCHANGE_URL to use the Exchange that the Horizon Agent is configured for. Specific error is: %v", apiMethod, err)
	}
}

// ExchangeGet runs a GET to the exchange api and fills in the specified json structure. If the structure is just a string, fill in the raw json.
// If the list of goodHttpCodes is not empty and none match the actual http code, it will exit with an error. Otherwise the actual code is returned.
func ExchangeGet(urlBase string, urlSuffix string, credentials string, goodHttpCodes []int, structure interface{}) (httpCode int) {
	url := urlBase + "/" + urlSuffix
	apiMsg := http.MethodGet + " " + url
	Verbose(apiMsg)
	httpClient := &http.Client{}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		Fatal(HTTP_ERROR, "%s new request failed: %v", apiMsg, err)
	}
	req.Header.Add("Accept", "application/json")
	if credentials != "" {
		req.Header.Add("Authorization", fmt.Sprintf("Basic %v", base64.StdEncoding.EncodeToString([]byte(credentials))))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		printHorizonExchRestError(apiMsg, err)
	}
	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		Fatal(HTTP_ERROR, "failed to read body response from %s: %v", apiMsg, err)
	}
	httpCode = resp.StatusCode
	Verbose("HTTP code: %d", httpCode)
	if !isGoodCode(httpCode, goodHttpCodes) {
		Fatal(HTTP_ERROR, "bad HTTP code %d from %s, output: %s", httpCode, apiMsg, string(bodyBytes))
	}

	if len(bodyBytes) > 0 && structure != nil { // the DP front-end of exchange will return nothing when auth problem
		switch s := structure.(type) {
		case *[]byte:
			// This is the signal that they want the raw body back
			*s = bodyBytes
		case *string:
			// If the structure to fill in is just a string, unmarshal/remarshal it to get it in json indented form, and then return as a string
			//todo: this gets it in json indented form, but also returns the fields in random order (because they were interpreted as a map)
			var jsonStruct interface{}
			err = json.Unmarshal(bodyBytes, &jsonStruct)
			if err != nil {
				Fatal(JSON_PARSING_ERROR, "failed to unmarshal exchange body response from %s: %v", apiMsg, err)
			}
			jsonBytes, err := json.MarshalIndent(jsonStruct, "", JSON_INDENT)
			if err != nil {
				Fatal(JSON_PARSING_ERROR, "failed to marshal exchange output from %s: %v", apiMsg, err)
			}
			*s = string(jsonBytes)
		default:
			err = json.Unmarshal(bodyBytes, structure)
			if err != nil {
				Fatal(JSON_PARSING_ERROR, "failed to unmarshal exchange body response from %s: %v", apiMsg, err)
			}
		}
	}
	return
}

// ExchangePutPost runs a PUT or POST to the exchange api to create of update a resource. If body is a string, it will be given to the exchange
// as json. Otherwise the struct will be marshaled to json.
// If the list of goodHttpCodes is not empty and none match the actual http code, it will exit with an error. Otherwise the actual code is returned.
func ExchangePutPost(method string, urlBase string, urlSuffix string, credentials string, goodHttpCodes []int, body interface{}) (httpCode int) {
	url := urlBase + "/" + urlSuffix
	apiMsg := method + " " + url
	Verbose(apiMsg)
	if IsDryRun() {
		return 201
	}
	httpClient := &http.Client{}

	// Prepare body
	var jsonBytes []byte
	bodyIsBytes := false
	switch b := body.(type) {
	// If the body is a byte array, we treat it like a file being uploaded (not multi-part)
	case []byte:
		jsonBytes = b
		bodyIsBytes = true
	case string:
		jsonBytes = []byte(b)
	default:
		var err error
		jsonBytes, err = json.Marshal(body)
		if err != nil {
			Fatal(JSON_PARSING_ERROR, "failed to marshal exchange body for %s: %v", apiMsg, err)
		}
	}
	requestBody := bytes.NewBuffer(jsonBytes)

	// Create the request and run it
	req, err := http.NewRequest(method, url, requestBody)
	if err != nil {
		Fatal(HTTP_ERROR, "%s new request failed: %v", apiMsg, err)
	}
	req.Header.Add("Accept", "application/json")
	if bodyIsBytes {
		req.Header.Add("Content-Length", strconv.Itoa(len(jsonBytes)))
	} else {
		req.Header.Add("Content-Type", "application/json")
	}
	if credentials != "" {
		req.Header.Add("Authorization", fmt.Sprintf("Basic %v", base64.StdEncoding.EncodeToString([]byte(credentials))))
	} // else it is an anonymous call
	resp, err := httpClient.Do(req)
	if err != nil {
		printHorizonExchRestError(apiMsg, err)
	}
	defer resp.Body.Close()
	httpCode = resp.StatusCode
	Verbose("HTTP code: %d", httpCode)
	if !isGoodCode(httpCode, goodHttpCodes) {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			Fatal(HTTP_ERROR, "failed to read exchange body response from %s: %v", apiMsg, err)
		}
		respMsg := exchange.PostDeviceResponse{}
		err = json.Unmarshal(bodyBytes, &respMsg)
		if err != nil {
			Fatal(HTTP_ERROR, "bad HTTP code %d from %s: %s", httpCode, apiMsg, string(bodyBytes))
		}
		Fatal(HTTP_ERROR, "bad HTTP code %d from %s: %s, %s", httpCode, apiMsg, respMsg.Code, respMsg.Msg)
	}
	return
}

// ExchangeDelete deletes a resource via the exchange api.
// If the list of goodHttpCodes is not empty and none match the actual http code, it will exit with an error. Otherwise the actual code is returned.
func ExchangeDelete(urlBase string, urlSuffix string, credentials string, goodHttpCodes []int) (httpCode int) {
	url := urlBase + "/" + urlSuffix
	apiMsg := http.MethodDelete + " " + url
	Verbose(apiMsg)
	if IsDryRun() {
		return 204
	}
	httpClient := &http.Client{}
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		Fatal(HTTP_ERROR, "%s new request failed: %v", apiMsg, err)
	}
	req.Header.Add("Authorization", fmt.Sprintf("Basic %v", base64.StdEncoding.EncodeToString([]byte(credentials))))
	resp, err := httpClient.Do(req)
	if err != nil {
		printHorizonExchRestError(apiMsg, err)
	}
	// delete never returns a body
	httpCode = resp.StatusCode
	Verbose("HTTP code: %d", httpCode)
	if !isGoodCode(httpCode, goodHttpCodes) {
		Fatal(HTTP_ERROR, "bad HTTP code %d from %s", httpCode, apiMsg)
	}
	return
}

func ConvertTime(unixSeconds uint64) string {
	if unixSeconds == 0 {
		return ""
	}
	return time.Unix(int64(unixSeconds), 0).String()
}

// find correct credentials to use. Use -u or -n if one of them is not empty.
// If both are empty, use HZN_EXCHANGE_USER_AUTH first, if it is not set use HZN_EXCHANGE_NODE_AUTH.
func GetExchangeAuth(userPw string, nodeIdTok string) string {
	credToUse := ""

	if userPw != "" {
		credToUse = userPw
	} else {
		if nodeIdTok != "" {
			credToUse = nodeIdTok
		} else {
			if tmpU := WithDefaultEnvVar(&userPw, "HZN_EXCHANGE_USER_AUTH"); *tmpU != "" {
				credToUse = *tmpU
			} else if tmpN := WithDefaultEnvVar(&nodeIdTok, "HZN_EXCHANGE_NODE_AUTH"); *tmpN != "" {
				credToUse = *tmpN
			}
		}
	}

	if credToUse == "" {
		Fatal(CLI_INPUT_ERROR, "exchange authentication must be specified with one of the following: the -u flag, the -n flag, HZN_EXCHANGE_USER_AUTH or HZN_EXCHANGE_NODE_AUTH")
	}

	return credToUse
}

// set env variable ARCH if it is not set
func SetDefaultArch() {
	arch := os.Getenv("ARCH")
	if arch == "" {
		os.Setenv("ARCH", runtime.GOARCH)
	}
}

// get the default private or public key file name
func GetDefaultSigningKeyFile(isPublic bool) (string, error) {
	if c_user, err := user.Current(); err != nil {
		return "", fmt.Errorf("Failed to get current os user. %v", err)
	} else {
		if isPublic {
			return filepath.Join(c_user.HomeDir, DEFAULT_PUBLIC_KEY_FILE), nil
		} else {
			return filepath.Join(c_user.HomeDir, DEFAULT_PRIVATE_KEY_FILE), nil
		}
	}
}

// Gets default keys if not set, verify key files exist.
func VerifySigningKeyInput(keyFile string, isPublic bool) string {
	var err error
	// get default file names if input is empty
	if keyFile == "" {
		if keyFile, err = GetDefaultSigningKeyFile(isPublic); err != nil {
			Fatal(CLI_GENERAL_ERROR, err.Error())
		}
	}

	// convert to absolute path
	if keyFile, err = filepath.Abs(keyFile); err != nil {
		Fatal(CLI_GENERAL_ERROR, "Failed to get absolute path for file %v. %v", keyFile, err)
	}

	// check file exist
	if _, err := os.Stat(keyFile); os.IsNotExist(err) {
		Fatal(CLI_GENERAL_ERROR, "%v. Please create the signing key.", err)
	}
	return keyFile
}

/* Do not need at the moment, but keeping for reference...
// Run a command with optional stdin and args, and return stdout, stderr
func RunCmd(stdinBytes []byte, commandString string, args ...string) ([]byte, []byte) {
	// For debug, build the full cmd string
	cmdStr := commandString
	for _, a := range args {
		cmdStr += " " + a
	}
	if stdinBytes != nil { cmdStr += " < stdin" }
	Verbose("running: %v\n", cmdStr)

	// Create the command object with its args
	cmd := exec.Command(commandString, args...)
	if cmd == nil { Fatal(EXEC_CMD_ERROR, "did not get a command object") }

	var stdin io.WriteCloser
	//var jInbytes []byte
	var err error
	if stdinBytes != nil {
		// Create the std in pipe
		stdin, err = cmd.StdinPipe()
		if err != nil { Fatal(EXEC_CMD_ERROR, "Could not get Stdin pipe, error: %v", err) }
		// Read the input file
		//jInbytes, err = ioutil.ReadFile(stdinFilename)
		//if err != nil { Fatal(EXEC_CMD_ERROR,"Unable to read " + stdinFilename + " file, error: %v", err) }
	}
	// Create the stdout pipe to hold the output from the command
	stdout, err := cmd.StdoutPipe()
	if err != nil { Fatal(EXEC_CMD_ERROR,"could not retrieve output from command, error: %v", err) }
	// Create the stderr pipe to hold the errors from the command
	stderr, err := cmd.StderrPipe()
	if err != nil { Fatal(EXEC_CMD_ERROR,"could not retrieve stderr from command, error: %v", err) }

	// Start the command, which will block for input from stdin if the cmd reads from it
	err = cmd.Start()
	if err != nil { Fatal(EXEC_CMD_ERROR,"Unable to start command, error: %v", err) }

	if stdinBytes != nil {
		// Send in the std in bytes
		_, err = stdin.Write(stdinBytes)
		if err != nil { Fatal(EXEC_CMD_ERROR, "Unable to write to stdin of command, error: %v", err) }
		// Close std in so that the command will begin to execute
		err = stdin.Close()
		if err != nil { Fatal(EXEC_CMD_ERROR, "Unable to close stdin, error: %v", err) }
	}

	err = error(nil)
	// Read the output from stdout and stderr into byte arrays
	// stdoutBytes, err := readPipe(stdout)
	stdoutBytes, err := ioutil.ReadAll(stdout)
	if err != nil { Fatal(EXEC_CMD_ERROR,"could not read stdout, error: %v", err) }
	// stderrBytes, err := readPipe(stderr)
	stderrBytes, err := ioutil.ReadAll(stderr)
	if err != nil { Fatal(EXEC_CMD_ERROR,"could not read stderr, error: %v", err) }

	// Now block waiting for the command to complete
	err = cmd.Wait()
	if err != nil { Fatal(EXEC_CMD_ERROR, "command failed: %v, stderr: %s", err, string(stderrBytes)) }

	return stdoutBytes, stderrBytes
}
*/

/* Will probably need this....
func getString(v interface{}) string {
	if reflect.ValueOf(v).IsNil() { return "" }
	return fmt.Sprintf("%v", reflect.Indirect(reflect.ValueOf(v)))
}
*/

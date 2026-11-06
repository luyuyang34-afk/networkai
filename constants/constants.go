package constants

import "os"

const (
    USER_INDEX = "user"
    POST_INDEX = "post"
    ES_URL = "http://10.162.0.2:9200"
)

// Credentials come from environment variables so they never get committed.
// Set them with: source ~/.networkai.env
var (
    ES_USERNAME = os.Getenv("ES_USERNAME")
    ES_PASSWORD = os.Getenv("ES_PASSWORD")
)

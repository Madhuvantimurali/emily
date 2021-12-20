package emily

import(
	"log"
	"fmt"
)

// Wrapper for debug level logging using the RACE Logging API call
func LogDebug(msg ...interface{}) {
	log.Println("[raven-debug]", fmt.Sprint(msg...), "")
}

// Wrapper for info level logging using the RACE Logging API call
func LogInfo(msg ...interface{}) {
	log.Println("[raven-info]", fmt.Sprint(msg...), "")
}

// Wrapper for warn level logging using the RACE Logging API call
func LogWarning(msg ...interface{}) {
	log.Println("[raven-info]", fmt.Sprint(msg...), "")
}

// Wrapper for error level logging using the RACE Logging API call
func LogError(msg ...interface{}) {
	log.Fatalln("[raven-error]", fmt.Sprint(msg...), "")
}

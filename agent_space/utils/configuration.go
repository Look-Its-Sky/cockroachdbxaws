package utils

import (
	"log"

	"github.com/joho/godotenv"
)

// loads .env into the environment, current directory first then the parent
func LoadConfig() {
	// Try loading from current directory
	errLocal := godotenv.Load(".env")

	// Try loading from parent directory
	errParent := godotenv.Load("../.env")

	if errLocal != nil && errParent != nil {
		log.Println("No .env or ../.env file found. Relying on system environment variables.")
	}
}

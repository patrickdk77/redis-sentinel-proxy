package main

import (
	"os"
	"context"

	"github.com/redis/go-redis/v9"
)

var ctx = context.Background()

func main() {
	os.Exit(check(settings()))
}

func settings() (string, string, string) {
	host := os.Getenv("LISTEN")
	if len(host) == 0 {
		host = "localhost:9999"
	}
        user := os.Getenv("USERNAME")
	pass := os.Getenv("REDIS_PASSWORD")
	if len(pass) == 0 {
		pass = os.Getenv("PASSWORD")
	}
	return host, user, pass
}

func check(host, user, pass string) int {
	client := redis.NewClient(&redis.Options{ Addr: host, Username: user, Password: pass, })
	defer client.Close()
	role, err := client.Do(ctx,"role").Result()
	if err != nil {
		return 1
	}
	status, ok := role.([]interface{})
	if !ok || len(status) == 0 {
		return 1
	}
	if status[0] == "master" {
		return 0
	}
	return 127
}

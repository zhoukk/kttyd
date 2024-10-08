package main

import (
	"log"
	"time"

	"github.com/zhoukk/kttyd"
)

func main() {
	tty, err := kttyd.NewKttyd(":0", "bash", []string{})
	if err != nil {
		log.Println(err)
	}

	tty.SetBasicAuth("admin:admin").SetTitle("test@kttyd").SetWorkdir("./").SetLogger(func(err error) {
		log.Println(err)
	})

	log.Printf("http://127.0.0.1:%s%s", tty.Port, tty.Path)

	go func() {
		err = tty.Start()
		if err != nil {
			log.Println(err)
		}
	}()

	time.Sleep(100 * time.Second)

	tty.Stop()

	log.Println("tty stop")
}

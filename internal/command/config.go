package command

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/rhobuild/runpool/internal/config"
)

func runConfigValidate(streams IO, file string) error {
	if file == "" {
		return usagef("config validate: --file is required")
	}
	if _, err := config.LoadFile(file); err != nil {
		return err
	}
	fmt.Fprintln(streams.Out, "configuration valid")
	return nil
}

func runConfigEffective(streams IO, file string) error {
	var (
		c   *config.Config
		err error
	)
	if file != "" {
		c, err = config.LoadFile(file)
	} else {
		c, err = config.Load(os.Getenv)
	}
	if err != nil {
		return err
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	_, err = streams.Out.Write(out)
	return err
}

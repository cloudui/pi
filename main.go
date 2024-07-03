package main

import (
	"context"
	"fmt"
	"os/exec"
	picommon "pi-module/common"

	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/module"
	"go.viam.com/utils"
)

func main() {
	utils.ContextualMain(mainWithArgs, module.NewLoggerFromArgs("custom-pi"))
}

func mainWithArgs(ctx context.Context, args []string, logger logging.Logger) error {
	// start up pigpiod daemon
	err := startPigpiod()
	if err != nil {
		fmt.Printf("Error: %s\n", err)
	} else {
		fmt.Println("pigpiod started successfully")
	}

	pigpio, err := module.NewModuleFromArgs(ctx, logger)
	if err != nil {
		return err
	}
	pigpio.AddModelFromRegistry(ctx, board.API, picommon.Model)

	pigpio.Start(ctx)

	defer pigpio.Close(ctx)

	stopPigpiod()

	<-ctx.Done()
	return nil
}

func startPigpiod() error {
	cmd := exec.Command("sudo", "pigpiod")
	err := cmd.Run()

	return err
}

func stopPigpiod() error {
	cmd := exec.Command("sudo", "killall", "pigpiod")
	err := cmd.Run()

	return err
}

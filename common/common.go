// go:build linux && (arm64 || arm) && !no_pigpio && !no_cgo

// Package picommon contains shared information for supported and non-supported pi boards.
package picommon

// #include <stdlib.h>
// #include <pigpiod_if2.h>
// #include "pi.h"
// #cgo LDFLAGS: -lpigpiod_if2
import "C"

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pkg/errors"

	pb "go.viam.com/api/component/board/v1"
	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/board/pinwrappers"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// Model is the name used refer to any implementation of a pi based component.
var Model = resource.NewModel("viam-labs", "board", "rpi4-daemon")

// A Config describes the configuration of a board and all of its connected parts.
type Config struct {
	// AnalogReaders     []mcp3008helper.MCP3008AnalogConfig `json:"analogs,omitempty"`
	// DigitalInterrupts []DigitalInterruptConfig            `json:"digital_interrupts,omitempty"`
}

// init registers a pi board based on pigpio.
func init() {
	resource.RegisterComponent(
		board.API,
		Model,
		resource.Registration[board.Board, *Config]{
			Constructor: newDaemonPigpio,
		},
	)
}

type daemonBoard struct {
	resource.Named
	// To prevent deadlocks, we must never lock this mutex while instanceMu, defined below, is
	// locked. It's okay to lock instanceMu while this is locked, though. This invariant prevents
	// deadlocks if both mutexes are locked by separate goroutines and are each waiting to lock the
	// other as well.
	mu            sync.Mutex
	cancelCtx     context.Context
	cancelFunc    context.CancelFunc
	duty          int // added for mutex
	gpioConfigSet map[int]bool
	analogReaders map[string]*pinwrappers.AnalogSmoother
	// `interrupts` maps interrupt names to the interrupts. `interruptsHW` maps broadcom addresses
	// to these same values. The two should always have the same set of values.
	// interrupts   map[string]ReconfigurableDigitalInterrupt
	// interruptsHW map[uint]ReconfigurableDigitalInterrupt
	logger   logging.Logger
	isClosed bool
	piID     int

	activeBackgroundWorkers sync.WaitGroup
}

func newDaemonPigpio(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (board.Board, error) {
	pigpioID, err := initializePigpio()
	if err != nil {
		return nil, err
	}

	fmt.Println("PigpioID: ", pigpioID)

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	piInstance := &daemonBoard{
		Named:      conf.ResourceName().AsNamed(),
		logger:     logger,
		isClosed:   false,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,

		piID: pigpioID,
	}

	if err := piInstance.Reconfigure(ctx, nil, conf); err != nil {
		// This has to happen outside of the lock to avoid a deadlock with interrupts.
		C.pigpio_stop(C.int(pigpioID))
		instanceMu.Lock()
		pigpioInitialized = false
		instanceMu.Unlock()
		logger.CError(ctx, "Pi GPIO terminated due to failed init.")
		return nil, err
	}

	return piInstance, nil

}

var (
	pigpioInitialized bool
	// To prevent deadlocks, we must never lock the mutex of a specific piPigpio struct, above,
	// while this is locked. It is okay to lock this while one of those other mutexes is locked
	// instead.
	instanceMu sync.RWMutex
	instances  = map[*daemonBoard]struct{}{}
)

func initializePigpio() (int, error) {
	instanceMu.Lock()
	defer instanceMu.Unlock()

	if pigpioInitialized {
		return -1, nil
	}

	pid := C.my_pigpio_start()

	if pid < 0 {
		return -1, errors.New("gpio_start failed")
	}

	pigpioInitialized = true
	return int(pid), nil
}

func (pi *daemonBoard) Reconfigure(
	ctx context.Context,
	_ resource.Dependencies,
	conf resource.Config,
) error {
	_, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return err
	}

	pi.mu.Lock()
	defer pi.mu.Unlock()

	instanceMu.Lock()
	defer instanceMu.Unlock()
	instances[pi] = struct{}{}
	return nil
}

// AnalogByName returns an analog pin by name.
func (d *daemonBoard) AnalogByName(name string) (board.Analog, error) {
	return nil, nil
}

// DigitalInterruptByName returns a digital interrupt by name.
func (d *daemonBoard) DigitalInterruptByName(name string) (board.DigitalInterrupt, error) {
	return nil, nil

}

// GPIOPinByName returns a GPIOPin by name.
func (d *daemonBoard) GPIOPinByName(name string) (board.GPIOPin, error) {
	return nil, nil

}

// AnalogNames returns the names of all known analog pins.
func (d *daemonBoard) AnalogNames() []string {
	return nil

}

// DigitalInterruptNames returns the names of all known digital interrupts.
func (d *daemonBoard) DigitalInterruptNames() []string {
	return nil

}

// SetPowerMode sets the board to the given power mode. If
// provided, the board will exit the given power mode after
// the specified duration.
func (d *daemonBoard) SetPowerMode(ctx context.Context, mode pb.PowerMode, duration *time.Duration) error {
	return nil

}

// StreamTicks starts a stream of digital interrupt ticks.
func (d *daemonBoard) StreamTicks(ctx context.Context, interrupts []board.DigitalInterrupt, ch chan board.Tick,
	extra map[string]interface{}) error {
	return nil
}

// Validate ensures all parts of the config are valid.
func (conf *Config) Validate(path string) ([]string, error) {
	return nil, nil
}

// Close attempts to close all parts of the board cleanly.
func (pi *daemonBoard) Close(ctx context.Context) error {
	var terminate bool
	// Prevent duplicate calls to Close a board as this may overlap with
	// the reinitialization of the board
	pi.mu.Lock()
	var err error
	defer pi.mu.Unlock()
	if pi.isClosed {
		pi.logger.Info("Duplicate call to close pi board detected, skipping")
		return nil
	}
	pi.cancelFunc()
	pi.activeBackgroundWorkers.Wait()

	instanceMu.Lock()
	if len(instances) == 1 {
		terminate = true
		fmt.Println("Terminate is true")
	}
	delete(instances, pi)

	if terminate {
		pigpioInitialized = false
		instanceMu.Unlock()
		// This has to happen outside of the lock to avoid a deadlock with interrupts.
		C.pigpio_stop(C.int(pi.piID))
		pi.logger.CDebug(ctx, "Pi GPIO terminated properly.")
	} else {
		instanceMu.Unlock()
	}

	pi.isClosed = true
	return err
}

// GetGPIOBcom gets the level of the given broadcom pin
func (pi *daemonBoard) GetGPIOBcom(bcom int) (bool, error) {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	if !pi.gpioConfigSet[bcom] {
		if pi.gpioConfigSet == nil {
			pi.gpioConfigSet = map[int]bool{}
		}
		res := C.set_mode(C.int(pi.piID), C.uint(bcom), C.PI_INPUT)
		if res != 0 {
			return false, ConvertErrorCodeToMessage(int(res), "failed to set mode")
		}
		pi.gpioConfigSet[bcom] = true
	}

	// gpioRead retrns an int 1 or 0, we convert to a bool
	return C.gpio_read(C.int(pi.piID), C.uint(bcom)) != 0, nil
}

// SetGPIOBcom sets the given broadcom pin to high or low.
func (pi *daemonBoard) SetGPIOBcom(bcom int, high bool) error {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	if !pi.gpioConfigSet[bcom] {
		if pi.gpioConfigSet == nil {
			pi.gpioConfigSet = map[int]bool{}
		}
		res := C.set_mode(C.int(pi.piID), C.uint(bcom), C.PI_OUTPUT)
		if res != 0 {
			return ConvertErrorCodeToMessage(int(res), "failed to set mode")
		}
		pi.gpioConfigSet[bcom] = true
	}

	v := 0
	if high {
		v = 1
	}
	C.gpio_write(C.int(pi.piID), C.uint(bcom), C.uint(v))
	return nil
}

type gpioPin struct {
	pi   *daemonBoard
	bcom int
}

func (gp gpioPin) Set(ctx context.Context, high bool, extra map[string]interface{}) error {
	return gp.pi.SetGPIOBcom(gp.bcom, high)
}

func (gp gpioPin) Get(ctx context.Context, extra map[string]interface{}) (bool, error) {
	return gp.pi.GetGPIOBcom(gp.bcom)
}

//export pigpioInterruptCallback
func pigpioInterruptCallback(gpio, level int, rawTick uint32) {
	fmt.Println("Here!")
}

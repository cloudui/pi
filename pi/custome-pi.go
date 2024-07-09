// go:build linux && (arm64 || arm) && !no_pigpio && !no_cgo

// Package piimpl contains the implementation of a supported Raspberry Pi board.
package piimpl

/*
	This driver contains various functionalities of raspberry pi board using the
	pigpio library (https://abyz.me.uk/rpi/pigpio/pdif2.html).
	NOTE: This driver only supports software PWM functionality of raspberry pi.
		  For software PWM, we currently support the default sample rate of
		  5 microseconds, which supports the following 18 frequencies (Hz):
		  8000  4000  2000 1600 1000  800  500  400  320
          250   200   160  100   80   50   40   20   10
		  Details on this can be found here -> https://abyz.me.uk/rpi/pigpio/pdif2.html#set_PWM_frequency
*/

// #include <stdlib.h>
// #include "pigpiod_if2.h"
// #include "pi.h"

import "C"

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/multierr"
	pb "go.viam.com/api/component/board/v1"
	"go.viam.com/utils"

	"go.viam.com/rdk/components/board"
	"go.viam.com/rdk/components/board/mcp3008helper"
	picommon "go.viam.com/rdk/components/board/pi/common"
	"go.viam.com/rdk/components/board/pinwrappers"
	"go.viam.com/rdk/grpc"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	rdkutils "go.viam.com/rdk/utils"
)

// A Config describes the configuration of a board and all of its connected parts.
type Config struct {
	AnalogReaders     []mcp3008helper.MCP3008AnalogConfig `json:"analogs,omitempty"`
	DigitalInterrupts []DigitalInterruptConfig            `json:"digital_interrupts,omitempty"`
}

// Validate ensures all parts of the config are valid.
func (conf *Config) Validate(path string) ([]string, error) {
	return nil, nil
}

// piPigpio is an implementation of a board.Board of a Raspberry Pi
// accessed via pigpio.
type piPigpio struct {
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
	interrupts   map[string]ReconfigurableDigitalInterrupt
	interruptsHW map[uint]ReconfigurableDigitalInterrupt
	logger       logging.Logger
	isClosed     bool
	piID         int

	activeBackgroundWorkers sync.WaitGroup
}

var (
	pigpioInitialized bool
	// To prevent deadlocks, we must never lock the mutex of a specific piPigpio struct, above,
	// while this is locked. It is okay to lock this while one of those other mutexes is locked
	// instead.
	instanceMu sync.RWMutex
	instances  = map[*piPigpio]struct{}{}
)

func initializePigpio() (int, error) {
	instanceMu.Lock()
	defer instanceMu.Unlock()

	if pigpioInitialized {
		return -1, nil
	}

	// resetRes := C.resetDMAChannels()
	// if resetRes != 0 {
	// 	return errors.New("failed to reset DMA channels")
	// }

	// clearRes := C.clearDMAMemory()
	// if clearRes != 0 {
	// 	return errors.New("failed to clear DMA memory")
	// }

	pid := C.pigpio_start(C.NULL, C.NULL)

	if pid < 0 {
		return -1, errors.New("gpio_start failed")
	}

	pigpioInitialized = true
	return pid, nil
}

// newPigpio makes a new pigpio based Board using the given config.
func newPigpio(ctx context.Context, name resource.Name, cfg resource.Config, logger logging.Logger) (board.Board, error) {
	// this is so we can run it inside a daemon
	// internals := C.gpioCfgGetInternals()
	// internals |= C.PI_CFG_NOSIGHANDLER
	// resCode := C.gpioCfgSetInternals(internals)
	// if resCode < 0 {
	// 	return nil, picommon.ConvertErrorCodeToMessage(int(resCode), "gpioCfgSetInternals failed with code")
	// }

	pigpioID, err := initializePigpio()

	if err != nil {
		return nil, err
	}

	cancelCtx, cancelFunc := context.WithCancel(context.Background())
	piInstance := &piPigpio{
		Named:      name.AsNamed(),
		logger:     logger,
		isClosed:   false,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
		piID:       pigpioID,
	}

	if err := piInstance.Reconfigure(ctx, nil, cfg); err != nil {
		// This has to happen outside of the lock to avoid a deadlock with interrupts.
		C.gpio_stop(pigpioID)
		instanceMu.Lock()
		pigpioInitialized = false
		instanceMu.Unlock()
		logger.CError(ctx, "Pi GPIO terminated due to failed init.")
		return nil, err
	}
	return piInstance, nil
}

func (pi *piPigpio) Reconfigure(
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

// StreamTicks starts a stream of digital interrupt ticks.
func (pi *piPigpio) StreamTicks(ctx context.Context, interrupts []board.DigitalInterrupt, ch chan board.Tick,
	extra map[string]interface{},
) error {
	for _, i := range interrupts {
		AddCallback(i.(*BasicDigitalInterrupt), ch)
	}

	pi.activeBackgroundWorkers.Add(1)

	utils.ManagedGo(func() {
		// Wait until it's time to shut down then remove callbacks.
		select {
		case <-ctx.Done():
		case <-pi.cancelCtx.Done():
		}
		for _, i := range interrupts {
			RemoveCallback(i.(*BasicDigitalInterrupt), ch)
		}
	}, pi.activeBackgroundWorkers.Done)

	return nil
}

// GPIOPinByName returns a GPIOPin by name.
func (pi *piPigpio) GPIOPinByName(pin string) (board.GPIOPin, error) {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	bcom, have := broadcomPinFromHardwareLabel(pin)
	if !have {
		return nil, errors.Errorf("no hw pin for (%s)", pin)
	}
	return gpioPin{pi, int(bcom)}, nil
}

type gpioPin struct {
	pi   *piPigpio
	bcom int
}

func (gp gpioPin) Set(ctx context.Context, high bool, extra map[string]interface{}) error {
	return gp.pi.SetGPIOBcom(gp.bcom, high)
}

func (gp gpioPin) Get(ctx context.Context, extra map[string]interface{}) (bool, error) {
	return gp.pi.GetGPIOBcom(gp.bcom)
}

func (gp gpioPin) PWM(ctx context.Context, extra map[string]interface{}) (float64, error) {
	return gp.pi.pwmBcom(gp.bcom)
}

func (gp gpioPin) SetPWM(ctx context.Context, dutyCyclePct float64, extra map[string]interface{}) error {
	return gp.pi.SetPWMBcom(gp.bcom, dutyCyclePct)
}

func (gp gpioPin) PWMFreq(ctx context.Context, extra map[string]interface{}) (uint, error) {
	return gp.pi.pwmFreqBcom(gp.bcom)
}

func (gp gpioPin) SetPWMFreq(ctx context.Context, freqHz uint, extra map[string]interface{}) error {
	return gp.pi.SetPWMFreqBcom(gp.bcom, freqHz)
}

// GetGPIOBcom gets the level of the given broadcom pin
func (pi *piPigpio) GetGPIOBcom(bcom int) (bool, error) {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	if !pi.gpioConfigSet[bcom] {
		if pi.gpioConfigSet == nil {
			pi.gpioConfigSet = map[int]bool{}
		}
		res := C.set_mode(pi.piID, C.uint(bcom), C.PI_INPUT)
		if res != 0 {
			return false, picommon.ConvertErrorCodeToMessage(int(res), "failed to set mode")
		}
		pi.gpioConfigSet[bcom] = true
	}

	// gpioRead retrns an int 1 or 0, we convert to a bool
	return C.gpio_read(pi.piID, C.uint(bcom)) != 0, nil
}

// SetGPIOBcom sets the given broadcom pin to high or low.
func (pi *piPigpio) SetGPIOBcom(bcom int, high bool) error {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	if !pi.gpioConfigSet[bcom] {
		if pi.gpioConfigSet == nil {
			pi.gpioConfigSet = map[int]bool{}
		}
		res := C.set_mode(pi.piID, C.uint(bcom), C.PI_OUTPUT)
		if res != 0 {
			return picommon.ConvertErrorCodeToMessage(int(res), "failed to set mode")
		}
		pi.gpioConfigSet[bcom] = true
	}

	v := 0
	if high {
		v = 1
	}
	C.gpio_write(pi.piID, C.uint(bcom), C.uint(v))
	return nil
}

func (pi *piPigpio) pwmBcom(bcom int) (float64, error) {
	res := C.get_PWM_dutycycle(pi.piID, C.uint(bcom))
	return float64(res) / 255, nil
}

// SetPWMBcom sets the given broadcom pin to the given PWM duty cycle.
func (pi *piPigpio) SetPWMBcom(bcom int, dutyCyclePct float64) error {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	dutyCycle := rdkutils.ScaleByPct(255, dutyCyclePct)
	pi.duty = int(C.set_PWM_dutycycle(pi.piID, C.uint(bcom), C.uint(dutyCycle)))
	if pi.duty != 0 {
		return errors.Errorf("pwm set fail %d", pi.duty)
	}
	return nil
}

func (pi *piPigpio) pwmFreqBcom(bcom int) (uint, error) {
	res := C.get_PWM_frequency(pi.piID, C.uint(bcom))
	return uint(res), nil
}

// SetPWMFreqBcom sets the given broadcom pin to the given PWM frequency.
func (pi *piPigpio) SetPWMFreqBcom(bcom int, freqHz uint) error {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	if freqHz == 0 {
		freqHz = 800 // Original default from libpigpio
	}
	newRes := C.set_PWM_frequency(pi.piID, C.uint(bcom), C.uint(freqHz))

	if newRes == C.PI_BAD_USER_GPIO {
		return picommon.ConvertErrorCodeToMessage(int(newRes), "pwm set freq failed")
	}

	if newRes != C.int(freqHz) {
		pi.logger.Infof("cannot set pwm freq to %d, setting to closest freq %d", freqHz, newRes)
	}
	return nil
}

// AnalogNames returns the names of all known analog pins.
func (pi *piPigpio) AnalogNames() []string {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	names := []string{}
	for k := range pi.analogReaders {
		names = append(names, k)
	}
	return names
}

// DigitalInterruptNames returns the names of all known digital interrupts.
func (pi *piPigpio) DigitalInterruptNames() []string {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	names := []string{}
	for k := range pi.interrupts {
		names = append(names, k)
	}
	return names
}

// AnalogByName returns an analog pin by name.
func (pi *piPigpio) AnalogByName(name string) (board.Analog, error) {
	pi.mu.Lock()
	defer pi.mu.Unlock()
	a, ok := pi.analogReaders[name]
	if !ok {
		return nil, errors.Errorf("can't find Analog pin (%s)", name)
	}
	return a, nil
}

func (pi *piPigpio) SetPowerMode(ctx context.Context, mode pb.PowerMode, duration *time.Duration) error {
	return grpc.UnimplementedError
}

// Close attempts to close all parts of the board cleanly.
func (pi *piPigpio) Close(ctx context.Context) error {
	var terminate bool
	// Prevent duplicate calls to Close a board as this may overlap with
	// the reinitialization of the board
	pi.mu.Lock()
	defer pi.mu.Unlock()
	if pi.isClosed {
		pi.logger.Info("Duplicate call to close pi board detected, skipping")
		return nil
	}
	pi.cancelFunc()
	pi.activeBackgroundWorkers.Wait()

	var err error
	for _, analog := range pi.analogReaders {
		err = multierr.Combine(err, analog.Close(ctx))
	}
	pi.analogReaders = map[string]*pinwrappers.AnalogSmoother{}

	for bcom := range pi.interruptsHW {
		if result := C.teardownInterrupt(C.int(bcom)); result != 0 {
			err = multierr.Combine(err, picommon.ConvertErrorCodeToMessage(int(result), "error"))
		}
	}
	pi.interrupts = map[string]ReconfigurableDigitalInterrupt{}
	pi.interruptsHW = map[uint]ReconfigurableDigitalInterrupt{}

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
		C.pigpio_stop(pi.piID)
		pi.logger.CDebug(ctx, "Pi GPIO terminated properly.")
	} else {
		instanceMu.Unlock()
	}

	pi.isClosed = true
	return err
}

var (
	lastTick      = uint32(0)
	tickRollevers = 0
)

//export pigpioInterruptCallback
func pigpioInterruptCallback(gpio, level int, rawTick uint32) {
	if rawTick < lastTick {
		tickRollevers++
	}
	lastTick = rawTick

	tick := (uint64(tickRollevers) * uint64(math.MaxUint32)) + uint64(rawTick)

	instanceMu.RLock()
	defer instanceMu.RUnlock()
	for instance := range instances {
		i := instance.interruptsHW[uint(gpio)]
		if i == nil {
			logging.Global().Infof("no DigitalInterrupt configured for gpio %d", gpio)
			continue
		}
		high := true
		if level == 0 {
			high = false
		}
		// this should *not* block for long otherwise the lock
		// will be held
		switch di := i.(type) {
		case *BasicDigitalInterrupt:
			err := Tick(instance.cancelCtx, di, high, tick*1000)
			if err != nil {
				instance.logger.Error(err)
			}
		case *ServoDigitalInterrupt:
			err := ServoTick(instance.cancelCtx, di, high, tick*1000)
			if err != nil {
				instance.logger.Error(err)
			}
		default:
			instance.logger.Error("unknown digital interrupt type")
		}
	}
}

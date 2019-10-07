// This is a echo console running on the device UART.
// Connect using default baudrate for this hardware, 8-N-1 with your terminal program.
package main

import (
	"machine"
	"time"
)

// change these to test a different UART or pins if available
var (
	uart = machine.UART0
	tx   = machine.UART_TX_PIN
	rx   = machine.UART_RX_PIN
)

func keepAlive() {
	for {
		time.Sleep(10 * time.Millisecond)
	}
}

func main() {
	machine.LED.Configure(machine.PinConfig{Mode: machine.PinOutput})
	time.Sleep(time.Second)
	time.Sleep(time.Second)
	time.Sleep(time.Second)
	time.Sleep(time.Second)
	time.Sleep(time.Second)
	time.Sleep(time.Second)
	time.Sleep(time.Second)
	time.Sleep(time.Second)

	//uart.Configure(machine.UARTConfig{TX: tx, RX: rx})
	uart.Write([]byte("Echo console enabled. Type something then press enter:\r\n"))

	go keepAlive()

	input := make([]byte, 64)
	i := 0
	for {
		uart.Cond.Wait()
		uart.Cond.Clear()
		for uart.Buffered() > 0 {
			data, _ := uart.ReadByte()

			switch data {
			case 13:
				// return key
				uart.Write([]byte("\r\n"))
				uart.Write([]byte("You typed: "))
				uart.Write(input[:i])
				uart.Write([]byte("\r\n"))
				i = 0
			default:
				// just echo the character
				uart.WriteByte(data)
				input[i] = data
				i++
			}
		}
	}
}

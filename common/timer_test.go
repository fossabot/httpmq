package common

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestIntervalTimerOneShot(t *testing.T) {
	assert := assert.New(t)

	wg := sync.WaitGroup{}
	defer wg.Wait()
	ctxt, cancel := context.WithCancel(context.Background())
	defer cancel()
	uut, err := GetIntervalTimerInstance("testing", ctxt, &wg)
	assert.Nil(err)

	value := 0
	callback := func() error {
		value++
		return nil
	}

	assert.Nil(uut.Start(time.Millisecond*10, callback, true))
	time.Sleep(time.Millisecond * 12)
	assert.Equal(1, value)

	time.Sleep(time.Millisecond * 10)
	assert.Equal(1, value)

	assert.Nil(uut.Start(time.Millisecond*5, callback, true))
	time.Sleep(time.Millisecond * 6)
	assert.Equal(2, value)
}

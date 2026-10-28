//go:build (linux || darwin) && (amd64 || arm64)

package edc

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

type grepONNX struct {
	library                                                                                              uintptr
	env, options, session, memory                                                                        unsafe.Pointer
	createTensor                                                                                         func(unsafe.Pointer, unsafe.Pointer, uintptr, *int64, uintptr, int32, *unsafe.Pointer) unsafe.Pointer
	run                                                                                                  func(unsafe.Pointer, unsafe.Pointer, **byte, *unsafe.Pointer, uintptr, **byte, uintptr, *unsafe.Pointer) unsafe.Pointer
	data                                                                                                 func(unsafe.Pointer, *unsafe.Pointer) unsafe.Pointer
	shapeInfo                                                                                            func(unsafe.Pointer, *unsafe.Pointer) unsafe.Pointer
	dimensionsCount                                                                                      func(unsafe.Pointer, *uintptr) unsafe.Pointer
	dimensions                                                                                           func(unsafe.Pointer, *int64, uintptr) unsafe.Pointer
	errorMessage                                                                                         func(unsafe.Pointer) string
	releaseStatus, releaseValue, releaseShape, releaseSession, releaseOptions, releaseMemory, releaseEnv func(unsafe.Pointer)
}

func (o *grepONNX) check(status unsafe.Pointer) error {
	if status == nil {
		return nil
	}
	message := o.errorMessage(status)
	o.releaseStatus(status)
	return errors.New(message)
}

func newGrepONNX(libraryPath, modelPath string) (*grepONNX, error) {
	library, err := purego.Dlopen(libraryPath, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	o := &grepONNX{library: library}
	fail := func(err error) (*grepONNX, error) { o.close(); return nil, err }
	symbol, err := purego.Dlsym(library, "OrtGetApiBase")
	if err != nil {
		return fail(err)
	}
	var base func() unsafe.Pointer
	purego.RegisterFunc(&base, symbol)
	basePtr := base()
	if basePtr == nil {
		return fail(errors.New("ONNX Runtime API base unavailable"))
	}
	var getAPI func(uint32) unsafe.Pointer
	purego.RegisterFunc(&getAPI, *(*uintptr)(basePtr))
	api := getAPI(23)
	if api == nil {
		return fail(errors.New("ONNX Runtime API 23 unavailable"))
	}
	table := unsafe.Slice((*uintptr)(api), 101)
	// These C API slots are stable for ONNX Runtime API 23 and must match the pinned runtime.
	for _, index := range []int{2, 3, 6, 7, 9, 10, 24, 25, 49, 51, 61, 62, 65, 69, 92, 93, 94, 95, 96, 99, 100} {
		if table[index] == 0 {
			return fail(fmt.Errorf("ONNX Runtime API slot %d unavailable", index))
		}
	}
	var createEnv func(int32, string, *unsafe.Pointer) unsafe.Pointer
	var disableTelemetry func(unsafe.Pointer) unsafe.Pointer
	var createOptions func(*unsafe.Pointer) unsafe.Pointer
	var intra, inter func(unsafe.Pointer, int32) unsafe.Pointer
	var createSession func(unsafe.Pointer, string, unsafe.Pointer, *unsafe.Pointer) unsafe.Pointer
	var createMemory func(int32, int32, *unsafe.Pointer) unsafe.Pointer
	purego.RegisterFunc(&o.errorMessage, table[2])
	purego.RegisterFunc(&o.releaseStatus, table[93])
	purego.RegisterFunc(&o.releaseEnv, table[92])
	purego.RegisterFunc(&o.releaseMemory, table[94])
	purego.RegisterFunc(&o.releaseSession, table[95])
	purego.RegisterFunc(&o.releaseValue, table[96])
	purego.RegisterFunc(&o.releaseShape, table[99])
	purego.RegisterFunc(&o.releaseOptions, table[100])
	purego.RegisterFunc(&createEnv, table[3])
	purego.RegisterFunc(&disableTelemetry, table[6])
	purego.RegisterFunc(&createOptions, table[10])
	purego.RegisterFunc(&intra, table[24])
	purego.RegisterFunc(&inter, table[25])
	purego.RegisterFunc(&createSession, table[7])
	purego.RegisterFunc(&createMemory, table[69])
	purego.RegisterFunc(&o.createTensor, table[49])
	purego.RegisterFunc(&o.run, table[9])
	purego.RegisterFunc(&o.data, table[51])
	purego.RegisterFunc(&o.shapeInfo, table[65])
	purego.RegisterFunc(&o.dimensionsCount, table[61])
	purego.RegisterFunc(&o.dimensions, table[62])
	if err := o.check(createEnv(3, "edc", &o.env)); err != nil {
		return fail(err)
	}
	if err := o.check(disableTelemetry(o.env)); err != nil {
		return fail(err)
	}
	if err := o.check(createOptions(&o.options)); err != nil {
		return fail(err)
	}
	// 8코어 Apple Silicon 측정에서 8개가 4개보다 임베딩이 34% 빨랐고, 10개와 ORT 기본값은 효율 코어까지 써서 더 느렸다.
	threads := min(runtime.NumCPU(), 8)
	if threads < 1 {
		threads = 1
	}
	if err := o.check(intra(o.options, int32(threads))); err != nil {
		return fail(err)
	}
	if err := o.check(inter(o.options, 1)); err != nil {
		return fail(err)
	}
	if err := o.check(createSession(o.env, modelPath, o.options, &o.session)); err != nil {
		return fail(err)
	}
	if err := o.check(createMemory(1, 0, &o.memory)); err != nil {
		return fail(err)
	}
	return o, nil
}

func (o *grepONNX) close() {
	if o.memory != nil && o.releaseMemory != nil {
		o.releaseMemory(o.memory)
	}
	if o.session != nil && o.releaseSession != nil {
		o.releaseSession(o.session)
	}
	if o.options != nil && o.releaseOptions != nil {
		o.releaseOptions(o.options)
	}
	if o.env != nil && o.releaseEnv != nil {
		o.releaseEnv(o.env)
	}
	if o.library != 0 {
		purego.Dlclose(o.library)
	}
}

func (o *grepONNX) encode(batch [][]int64) ([][]float32, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	width := 0
	for _, row := range batch {
		if len(row) > width {
			width = len(row)
		}
	}
	if width == 0 || width > 128 {
		return nil, errors.New("invalid token length")
	}
	ids := make([]int64, len(batch)*width)
	mask := make([]int64, len(ids))
	types := make([]int64, len(ids))
	shape := []int64{int64(len(batch)), int64(width)}
	for i, row := range batch {
		copy(ids[i*width:], row)
		for j := range row {
			mask[i*width+j] = 1
		}
	}
	var pin runtime.Pinner
	pin.Pin(&ids[0])
	pin.Pin(&mask[0])
	pin.Pin(&types[0])
	defer pin.Unpin()
	inputs := make([]unsafe.Pointer, 3)
	defer func() {
		for _, v := range inputs {
			if v != nil {
				o.releaseValue(v)
			}
		}
	}()
	for i, data := range [][]int64{ids, mask, types} {
		if err := o.check(o.createTensor(o.memory, unsafe.Pointer(&data[0]), uintptr(len(data)*8), &shape[0], 2, 7, &inputs[i])); err != nil {
			return nil, err
		}
	}
	n1 := []byte("input_ids\x00")
	n2 := []byte("attention_mask\x00")
	n3 := []byte("token_type_ids\x00")
	n4 := []byte("last_hidden_state\x00")
	names := []*byte{&n1[0], &n2[0], &n3[0]}
	outputs := []*byte{&n4[0]}
	pin.Pin(&n1[0])
	pin.Pin(&n2[0])
	pin.Pin(&n3[0])
	pin.Pin(&n4[0])
	pin.Pin(&names[0])
	pin.Pin(&outputs[0])
	pin.Pin(&inputs[0])
	var output unsafe.Pointer
	if err := o.check(o.run(o.session, nil, &names[0], &inputs[0], 3, &outputs[0], 1, &output)); err != nil {
		return nil, err
	}
	if output == nil {
		return nil, errors.New("ONNX returned no output")
	}
	defer o.releaseValue(output)
	var info unsafe.Pointer
	if err := o.check(o.shapeInfo(output, &info)); err != nil {
		return nil, err
	}
	if info == nil {
		return nil, errors.New("ONNX output shape unavailable")
	}
	defer o.releaseShape(info)
	var count uintptr
	if err := o.check(o.dimensionsCount(info, &count)); err != nil {
		return nil, err
	}
	if count != 3 {
		return nil, errors.New("unexpected ONNX output rank")
	}
	dims := make([]int64, 3)
	if err := o.check(o.dimensions(info, &dims[0], 3)); err != nil {
		return nil, err
	}
	if dims[0] != int64(len(batch)) || dims[1] != int64(width) || dims[2] != 384 {
		return nil, errors.New("unexpected ONNX output shape")
	}
	var pointer unsafe.Pointer
	if err := o.check(o.data(output, &pointer)); err != nil {
		return nil, err
	}
	if pointer == nil {
		return nil, errors.New("ONNX output data unavailable")
	}
	values := unsafe.Slice((*float32)(pointer), len(batch)*width*384)
	vectors := make([][]float32, len(batch))
	for i, row := range batch {
		vector := make([]float32, 384)
		var norm float64
		for dimension := 0; dimension < 384; dimension++ {
			var sum float64
			for j := range row {
				sum += float64(values[(i*width+j)*384+dimension])
			}
			value := sum / float64(len(row))
			vector[dimension] = float32(value)
			norm += value * value
		}
		if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
			return nil, errors.New("invalid embedding")
		}
		scale := float32(1 / math.Sqrt(norm))
		for d := range vector {
			vector[d] *= scale
		}
		vectors[i] = vector
	}
	runtime.KeepAlive(ids)
	runtime.KeepAlive(mask)
	runtime.KeepAlive(types)
	runtime.KeepAlive(names)
	runtime.KeepAlive(outputs)
	return vectors, nil
}

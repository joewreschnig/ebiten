// Copyright 2022 The Ebiten Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package directx

import (
	"errors"
	"fmt"
	"reflect"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/hajimehoshi/ebiten/v2/internal/graphics"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver"
	"github.com/hajimehoshi/ebiten/v2/internal/shaderir"
)

const frameCount = 2

const is64bit = uint64(^uintptr(0)) == ^uint64(0)

// isDirectXAvailable indicates whether DirectX is available or not.
// In 32bit machines, DirectX is not used because
//   1) The functions syscall.Syscall cannot accept 64bit values as one argument
//   2) The struct layouts can be different
var isDirectXAvailable = is64bit && theGraphics.initializeDevice() == nil

var theGraphics Graphics

func Get() *Graphics {
	if !isDirectXAvailable {
		return nil
	}
	return &theGraphics
}

var inputElementDescs []_D3D12_INPUT_ELEMENT_DESC

func init() {
	position := []byte("POSITION\000")
	texcoord := []byte("TEXCOORD\000")
	color := []byte("COLOR\000")
	inputElementDescs = []_D3D12_INPUT_ELEMENT_DESC{
		{
			SemanticName:         &position[0],
			SemanticIndex:        0,
			Format:               _DXGI_FORMAT_R32G32_FLOAT,
			InputSlot:            0,
			AlignedByteOffset:    _D3D12_APPEND_ALIGNED_ELEMENT,
			InputSlotClass:       _D3D12_INPUT_CLASSIFICATION_PER_VERTEX_DATA,
			InstanceDataStepRate: 0,
		},
		{
			SemanticName:         &texcoord[0],
			SemanticIndex:        0,
			Format:               _DXGI_FORMAT_R32G32_FLOAT,
			InputSlot:            0,
			AlignedByteOffset:    _D3D12_APPEND_ALIGNED_ELEMENT,
			InputSlotClass:       _D3D12_INPUT_CLASSIFICATION_PER_VERTEX_DATA,
			InstanceDataStepRate: 0,
		},
		{
			SemanticName:         &color[0],
			SemanticIndex:        0,
			Format:               _DXGI_FORMAT_R32G32B32A32_FLOAT,
			InputSlot:            0,
			AlignedByteOffset:    _D3D12_APPEND_ALIGNED_ELEMENT,
			InputSlotClass:       _D3D12_INPUT_CLASSIFICATION_PER_VERTEX_DATA,
			InstanceDataStepRate: 0,
		},
	}
}

type Graphics struct {
	debug             *iD3D12Debug
	device            *iD3D12Device
	commandQueue      *iD3D12CommandQueue
	rtvDescriptorHeap *iD3D12DescriptorHeap
	rtvDescriptorSize uint32
	renderTargets     [frameCount]*iD3D12Resource1
	commandAllocators [frameCount]*iD3D12CommandAllocator
	fences            [frameCount]*iD3D12Fence
	fenceValues       [frameCount]uint64
	fenceWaitEvent    windows.Handle
	commandList       *iD3D12GraphicsCommandList
	debugCommandList  *iD3D12DebugCommandList
	vertices          [frameCount][]*iD3D12Resource1
	indices           [frameCount][]*iD3D12Resource1

	factory   *iDXGIFactory4
	adapter   *iDXGIAdapter1
	swapChain *iDXGISwapChain4

	window windows.HWND

	frameIndex int

	images         map[graphicsdriver.ImageID]*Image
	screenImage    *Image
	nextImageID    graphicsdriver.ImageID
	disposedImages [frameCount][]*Image

	pipelineStates
}

func (g *Graphics) initializeDevice() (ferr error) {
	if err := d3d12.Load(); err != nil {
		return err
	}

	// As g's lifetime is the same as the process's lifetime, debug and other objects are never released
	// if this initialization succeeds.

	d, err := d3D12GetDebugInterface()
	if err != nil {
		return err
	}
	g.debug = d
	defer func() {
		if ferr != nil {
			g.debug.Release()
			g.debug = nil
		}
	}()
	g.debug.EnableDebugLayer()

	// Enable the debugging.
	// TODO: Remove this before merging this branch.
	/*var debug3 *iD3D12Debug3
	g.debug.As(&debug3)
	debug3.SetEnableGPUBasedValidation(true)*/

	f, err := createDXGIFactory2(_DXGI_CREATE_FACTORY_DEBUG)
	if err != nil {
		return err
	}
	g.factory = f
	defer func() {
		if ferr != nil {
			g.factory.Release()
			g.factory = nil
		}
	}()

	if useWARP {
		a, err := g.factory.EnumWarpAdapter()
		if err != nil {
			return err
		}

		g.adapter = a
		defer func() {
			if ferr != nil {
				g.adapter.Release()
				g.adapter = nil
			}
		}()
	} else {
		for i := 0; ; i++ {
			a, err := g.factory.EnumAdapters1(uint32(i))
			if errors.Is(err, _DXGI_ERROR_NOT_FOUND) {
				break
			}
			if err != nil {
				return err
			}

			desc, err := a.GetDesc1()
			if err != nil {
				return err
			}
			if desc.Flags&_DXGI_ADAPTER_FLAG_SOFTWARE != 0 {
				a.Release()
				continue
			}
			if err := d3D12CreateDevice(unsafe.Pointer(a), _D3D_FEATURE_LEVEL_11_0, &_IID_ID3D12Device, nil); err != nil {
				a.Release()
				continue
			}
			g.adapter = a
			defer func() {
				if ferr != nil {
					g.adapter.Release()
					g.adapter = nil
				}
			}()
			break
		}
	}

	if g.adapter == nil {
		return errors.New("directx: DirectX 12 is not supported")
	}

	if err := d3D12CreateDevice(unsafe.Pointer(g.adapter), _D3D_FEATURE_LEVEL_11_0, &_IID_ID3D12Device, (*unsafe.Pointer)(unsafe.Pointer(&g.device))); err != nil {
		return err
	}

	return nil
}

func (g *Graphics) Initialize() (ferr error) {
	// Create an event for a fence.
	e, err := windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return fmt.Errorf("directx: CreateEvent failed: %w", err)
	}
	g.fenceWaitEvent = e

	// Create a command queue.
	desc := _D3D12_COMMAND_QUEUE_DESC{
		Type:  _D3D12_COMMAND_LIST_TYPE_DIRECT,
		Flags: _D3D12_COMMAND_QUEUE_FLAG_NONE,
	}
	c, err := g.device.CreateCommandQueue(&desc)
	if err != nil {
		return err
	}
	g.commandQueue = c
	defer func() {
		if ferr != nil {
			g.commandQueue.Release()
			g.commandQueue = nil
		}
	}()

	// Create command allocators.
	for i := 0; i < frameCount; i++ {
		ca, err := g.device.CreateCommandAllocator(_D3D12_COMMAND_LIST_TYPE_DIRECT)
		if err != nil {
			return err
		}
		g.commandAllocators[i] = ca
		defer func(i int) {
			if ferr != nil {
				g.commandAllocators[i].Release()
				g.commandAllocators[i] = nil
			}
		}(i)
	}

	// Create frame fences.
	for i := 0; i < frameCount; i++ {
		f, err := g.device.CreateFence(0, _D3D12_FENCE_FLAG_NONE)
		if err != nil {
			return err
		}
		g.fences[i] = f
		defer func(i int) {
			if ferr != nil {
				g.fences[i].Release()
				g.fences[i] = nil
			}
		}(i)
	}

	// Create a command list.
	cl, err := g.device.CreateCommandList(0, _D3D12_COMMAND_LIST_TYPE_DIRECT, g.commandAllocators[0], nil)
	if err != nil {
		return err
	}
	g.commandList = cl
	defer func() {
		if ferr != nil {
			g.commandList.Release()
			g.commandList = nil
		}
	}()

	// Close the command list once as this is immediately Reset at Begin.
	if err := g.commandList.Close(); err != nil {
		return err
	}

	// Enable the debugging.
	// TODO: Remove this before merging this branch.
	/*if err := g.commandList.QueryInterface(&_IID_ID3D12DebugCommandList, (*unsafe.Pointer)(unsafe.Pointer(&g.debugCommandList))); err != nil {
		return err
	}
	if err := g.debugCommandList.SetFeatureMask(_D3D12_DEBUG_FEATURE_CONSERVATIVE_RESOURCE_STATE_TRACKING); err != nil {
		return err
	}*/

	// Create a descriptor heap for RTV.
	h, err := g.device.CreateDescriptorHeap(&_D3D12_DESCRIPTOR_HEAP_DESC{
		Type:           _D3D12_DESCRIPTOR_HEAP_TYPE_RTV,
		NumDescriptors: frameCount,
		Flags:          _D3D12_DESCRIPTOR_HEAP_FLAG_NONE,
		NodeMask:       0,
	})
	if err != nil {
		return err
	}
	g.rtvDescriptorHeap = h
	defer func() {
		if ferr != nil {
			g.rtvDescriptorHeap.Release()
			g.rtvDescriptorHeap = nil
		}
	}()
	g.rtvDescriptorSize = g.device.GetDescriptorHandleIncrementSize(_D3D12_DESCRIPTOR_HEAP_TYPE_RTV)

	if err := g.pipelineStates.initialize(g.device); err != nil {
		return err
	}

	return nil
}

func createBuffer(device *iD3D12Device, bufferSize uint64) (*iD3D12Resource1, error) {
	r, err := device.CreateCommittedResource(&_D3D12_HEAP_PROPERTIES{
		Type:                 _D3D12_HEAP_TYPE_UPLOAD,
		CPUPageProperty:      _D3D12_CPU_PAGE_PROPERTY_UNKNOWN,
		MemoryPoolPreference: _D3D12_MEMORY_POOL_UNKNOWN,
		CreationNodeMask:     1,
		VisibleNodeMask:      1,
	}, _D3D12_HEAP_FLAG_NONE, &_D3D12_RESOURCE_DESC{
		Dimension:        _D3D12_RESOURCE_DIMENSION_BUFFER,
		Alignment:        0,
		Width:            bufferSize,
		Height:           1,
		DepthOrArraySize: 1,
		MipLevels:        1,
		Format:           _DXGI_FORMAT_UNKNOWN,
		SampleDesc: _DXGI_SAMPLE_DESC{
			Count:   1,
			Quality: 0,
		},
		Layout: _D3D12_TEXTURE_LAYOUT_ROW_MAJOR,
		Flags:  _D3D12_RESOURCE_FLAG_NONE,
	}, _D3D12_RESOURCE_STATE_GENERIC_READ, nil)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (g *Graphics) updateSwapChain(width, height int) error {
	if g.window == 0 {
		return errors.New("directx: the window handle is not initialized yet")
	}

	if g.swapChain == nil {
		if err := g.initSwapChain(width, height); err != nil {
			return err
		}
	} else {
		// TODO: Resize the chain buffer size if exists?
	}

	g.frameIndex = int(g.swapChain.GetCurrentBackBufferIndex())

	return nil
}

func (g *Graphics) initSwapChain(width, height int) (ferr error) {
	// Create a swap chain.
	s, err := g.factory.CreateSwapChainForHwnd(unsafe.Pointer(g.commandQueue), g.window, &_DXGI_SWAP_CHAIN_DESC1{
		Width:       uint32(width),
		Height:      uint32(height),
		Format:      _DXGI_FORMAT_R8G8B8A8_UNORM,
		BufferUsage: _DXGI_USAGE_RENDER_TARGET_OUTPUT,
		BufferCount: frameCount,
		SwapEffect:  _DXGI_SWAP_EFFECT_FLIP_DISCARD,
		SampleDesc: _DXGI_SAMPLE_DESC{
			Count:   1,
			Quality: 0,
		},
	}, nil, nil)
	if err != nil {
		return err
	}
	s.As(&g.swapChain)
	defer func() {
		if ferr != nil {
			g.swapChain.Release()
			g.swapChain = nil
		}
	}()

	// TODO: Call factory.MakeWindowAssociation not to support fullscreen transitions?
	// TODO: Get the current buffer index?

	// Create frame resources.
	h := g.rtvDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
	for i := 0; i < frameCount; i++ {
		r, err := g.swapChain.GetBuffer(uint32(i))
		if err != nil {
			return err
		}
		g.renderTargets[i] = r
		defer func(i int) {
			if ferr != nil {
				g.renderTargets[i].Release()
				g.renderTargets[i] = nil
			}
		}(i)

		g.device.CreateRenderTargetView(r, nil, h)
		h.Offset(1, g.rtvDescriptorSize)
	}

	return nil
}

func (g *Graphics) SetWindow(window uintptr) {
	g.window = windows.HWND(window)
	// TODO: need to update the swap chain?
}

func (g *Graphics) Begin() error {
	g.frameIndex = 0
	// The swap chain is initialized when NewScreenFramebufferImage is called.
	// This must be called at the first frame (between Begin and End).
	if g.swapChain != nil {
		g.frameIndex = int(g.swapChain.GetCurrentBackBufferIndex())
	}
	if err := g.commandAllocators[g.frameIndex].Reset(); err != nil {
		return err
	}
	if err := g.commandList.Reset(g.commandAllocators[g.frameIndex], nil); err != nil {
		return err
	}
	return nil
}

func (g *Graphics) End(present bool) error {
	if g.swapChain == nil {
		return fmt.Errorf("directx: the swap chain is not initialized yet at End")
	}

	if present {
		g.screenImage.transiteState(_D3D12_RESOURCE_STATE_PRESENT)
	}

	if err := g.commandList.Close(); err != nil {
		return err
	}
	g.commandQueue.ExecuteCommandLists([]*iD3D12GraphicsCommandList{g.commandList})

	if present {
		if err := g.swapChain.Present(1, 0); err != nil {
			return err
		}

		// Wait for the previous frame.
		fence := g.fences[g.frameIndex]
		g.fenceValues[g.frameIndex]++
		if err := g.commandQueue.Signal(fence, g.fenceValues[g.frameIndex]); err != nil {
			return err
		}

		nextIndex := (g.frameIndex + 1) % frameCount
		expected := g.fenceValues[nextIndex]
		actual := g.fences[nextIndex].GetCompletedValue()
		if actual < expected {
			if err := g.fences[nextIndex].SetEventOnCompletion(expected, g.fenceWaitEvent); err != nil {
				return err
			}
			const gpuWaitTimeout = 10 * 1000 // 10[s]
			if _, err := windows.WaitForSingleObject(g.fenceWaitEvent, gpuWaitTimeout); err != nil {
				return err
			}
		}

		g.vertices[nextIndex] = g.vertices[nextIndex][:0]
		g.indices[nextIndex] = g.indices[nextIndex][:0]

		for i, img := range g.disposedImages[nextIndex] {
			img.disposeImpl()
			g.disposedImages[nextIndex][i] = nil
		}
		g.disposedImages[nextIndex] = g.disposedImages[nextIndex][:0]
	}
	return nil
}

func (g *Graphics) SetTransparent(transparent bool) {
}

func (g *Graphics) SetVertices(vertices []float32, indices []uint16) (ferr error) {
	// Create buffers if necessary.
	vidx := len(g.vertices[g.frameIndex])
	if cap(g.vertices[g.frameIndex]) > vidx {
		g.vertices[g.frameIndex] = g.vertices[g.frameIndex][:vidx+1]
	} else {
		g.vertices[g.frameIndex] = append(g.vertices[g.frameIndex], nil)
	}
	if g.vertices[g.frameIndex][vidx] == nil {
		// TODO: Use the default heap for efficienty. See the official example HelloTriangle.
		vs, err := createBuffer(g.device, graphics.IndicesNum*graphics.VertexFloatNum*uint64(unsafe.Sizeof(float32(0))))
		if err != nil {
			return err
		}
		g.vertices[g.frameIndex][vidx] = vs
		defer func() {
			if ferr != nil {
				g.vertices[g.frameIndex][vidx].Release()
				g.vertices[g.frameIndex][vidx] = nil
			}
		}()
	}

	iidx := len(g.indices[g.frameIndex])
	if cap(g.indices[g.frameIndex]) > iidx {
		g.indices[g.frameIndex] = g.indices[g.frameIndex][:iidx+1]
	} else {
		g.indices[g.frameIndex] = append(g.indices[g.frameIndex], nil)
	}
	if g.indices[g.frameIndex][iidx] == nil {
		is, err := createBuffer(g.device, graphics.IndicesNum*uint64(unsafe.Sizeof(uint16(0))))
		if err != nil {
			return err
		}
		g.indices[g.frameIndex][iidx] = is
		defer func() {
			if ferr != nil {
				g.indices[g.frameIndex][iidx].Release()
				g.indices[g.frameIndex][iidx] = nil
			}
		}()
	}

	r := _D3D12_RANGE{0, 0}

	m, err := g.vertices[g.frameIndex][vidx].Map(0, &r)
	if err != nil {
		return err
	}
	copyFloat32s(m, vertices)
	if err := g.vertices[g.frameIndex][vidx].Unmap(0, nil); err != nil {
		return err
	}

	m, err = g.indices[g.frameIndex][iidx].Map(0, &r)
	if err != nil {
		return err
	}
	copyUint16s(m, indices)
	if err := g.indices[g.frameIndex][iidx].Unmap(0, nil); err != nil {
		return err
	}

	return nil
}

func (g *Graphics) NewImage(width, height int) (graphicsdriver.Image, error) {
	desc := _D3D12_RESOURCE_DESC{
		Dimension:        _D3D12_RESOURCE_DIMENSION_TEXTURE2D,
		Alignment:        0,
		Width:            uint64(graphics.InternalImageSize(width)),
		Height:           uint32(graphics.InternalImageSize(height)),
		DepthOrArraySize: 1,
		MipLevels:        1,
		Format:           _DXGI_FORMAT_R8G8B8A8_UNORM,
		SampleDesc: _DXGI_SAMPLE_DESC{
			Count:   1,
			Quality: 0,
		},
		Layout: _D3D12_TEXTURE_LAYOUT_UNKNOWN,
		Flags:  _D3D12_RESOURCE_FLAG_ALLOW_RENDER_TARGET,
	}

	state := _D3D12_RESOURCE_STATE_PIXEL_SHADER_RESOURCE
	t, err := g.device.CreateCommittedResource(&_D3D12_HEAP_PROPERTIES{
		Type:                 _D3D12_HEAP_TYPE_DEFAULT, // Upload?
		CPUPageProperty:      _D3D12_CPU_PAGE_PROPERTY_UNKNOWN,
		MemoryPoolPreference: _D3D12_MEMORY_POOL_UNKNOWN,
		CreationNodeMask:     1,
		VisibleNodeMask:      1,
	}, _D3D12_HEAP_FLAG_NONE, &desc, state, nil)
	if err != nil {
		return nil, err
	}

	layouts, numRows, rowSizeInBytes, totalBytes := g.device.GetCopyableFootprints(&desc, 0, 1, 0)

	i := &Image{
		graphics:       g,
		id:             g.genNextImageID(),
		width:          width,
		height:         height,
		texture:        t,
		state:          state,
		layouts:        layouts,
		numRows:        numRows,
		rowSizeInBytes: rowSizeInBytes,
		totalBytes:     totalBytes,
	}
	g.addImage(i)
	return i, nil
}

func (g *Graphics) NewScreenFramebufferImage(width, height int) (graphicsdriver.Image, error) {
	if err := g.updateSwapChain(width, height); err != nil {
		return nil, err
	}

	i := &Image{
		graphics: g,
		id:       g.genNextImageID(),
		width:    width,
		height:   height,
		screen:   true,
		state:    _D3D12_RESOURCE_STATE_PRESENT,
	}
	g.addImage(i)
	return i, nil
}

func (g *Graphics) addImage(img *Image) {
	if g.images == nil {
		g.images = map[graphicsdriver.ImageID]*Image{}
	}
	if _, ok := g.images[img.id]; ok {
		panic(fmt.Sprintf("directx: image ID %d was already registered", img.id))
	}
	g.images[img.id] = img
	if img.screen {
		g.screenImage = img
	}
}

func (g *Graphics) removeImage(img *Image) {
	delete(g.images, img.id)
	g.disposedImages[g.frameIndex] = append(g.disposedImages[g.frameIndex], img)
	if img.screen {
		g.screenImage = nil
	}
}

func (g *Graphics) SetVsyncEnabled(enabled bool) {
}

func (g *Graphics) SetFullscreen(fullscreen bool) {
}

func (g *Graphics) FramebufferYDirection() graphicsdriver.YDirection {
	return graphicsdriver.Downward
}

func (g *Graphics) NDCYDirection() graphicsdriver.YDirection {
	return graphicsdriver.Upward
}

func (g *Graphics) NeedsRestoring() bool {
	return false
}

func (g *Graphics) NeedsClearingScreen() bool {
	// TODO: Confirm this is really true.
	return true
}

func (g *Graphics) IsGL() bool {
	return false
}

func (g *Graphics) HasHighPrecisionFloat() bool {
	return true
}

func (g *Graphics) MaxImageSize() int {
	// TODO: Return a correct value.
	return 4096
}

func (g *Graphics) NewShader(program *shaderir.Program) (graphicsdriver.Shader, error) {
	// TODO: Implement this.
	return nil, nil
}

func (g *Graphics) DrawTriangles(dstID graphicsdriver.ImageID, srcs [graphics.ShaderImageNum]graphicsdriver.ImageID, offsets [graphics.ShaderImageNum - 1][2]float32, shaderID graphicsdriver.ShaderID, indexLen int, indexOffset int, mode graphicsdriver.CompositeMode, colorM graphicsdriver.ColorM, filter graphicsdriver.Filter, address graphicsdriver.Address, dstRegion, srcRegion graphicsdriver.Region, uniforms []graphicsdriver.Uniform, evenOdd bool) error {
	dst := g.images[dstID]

	if shaderID != graphicsdriver.InvalidShaderID {
		return fmt.Errorf("directx: shader is not implemented yet")
	}

	if err := dst.setAsRenderTarget(g.device); err != nil {
		return err
	}

	key := pipelineStatesKey{
		compositeMode: mode,
		screen:        dst.screen,
	}
	w, h := dst.internalSize()
	src := g.images[srcs[0]]
	src.transiteState(_D3D12_RESOURCE_STATE_PIXEL_SHADER_RESOURCE)
	if err := g.pipelineStates.useGraphicsPipelineState(g.device, g.commandList, g.frameIndex, key, float32(w), float32(h), src.resource()); err != nil {
		return err
	}

	g.commandList.RSSetViewports(1, &_D3D12_VIEWPORT{
		TopLeftX: 0,
		TopLeftY: 0,
		Width:    float32(w),
		Height:   float32(h),
		MinDepth: _D3D12_MIN_DEPTH,
		MaxDepth: _D3D12_MAX_DEPTH,
	})
	g.commandList.RSSetScissorRects(1, &_D3D12_RECT{
		left:   int32(dstRegion.X),
		top:    int32(dstRegion.Y),
		right:  int32(dstRegion.X + dstRegion.Width),
		bottom: int32(dstRegion.Y + dstRegion.Height),
	})

	g.commandList.IASetPrimitiveTopology(_D3D_PRIMITIVE_TOPOLOGY_TRIANGLELIST)
	g.commandList.IASetVertexBuffers(0, 1, &_D3D12_VERTEX_BUFFER_VIEW{
		BufferLocation: g.vertices[g.frameIndex][len(g.vertices[g.frameIndex])-1].GetGPUVirtualAddress(),
		SizeInBytes:    graphics.IndicesNum * graphics.VertexFloatNum * uint32(unsafe.Sizeof(float32(0))),
		StrideInBytes:  graphics.VertexFloatNum * uint32(unsafe.Sizeof(float32(0))),
	})
	g.commandList.IASetIndexBuffer(&_D3D12_INDEX_BUFFER_VIEW{
		BufferLocation: g.indices[g.frameIndex][len(g.indices[g.frameIndex])-1].GetGPUVirtualAddress(),
		SizeInBytes:    graphics.IndicesNum * uint32(unsafe.Sizeof(uint16(0))),
		Format:         _DXGI_FORMAT_R16_UINT,
	})

	g.commandList.DrawIndexedInstanced(uint32(indexLen), 1, uint32(indexOffset), 0, 0)

	return nil
}

func (g *Graphics) genNextImageID() graphicsdriver.ImageID {
	g.nextImageID++
	return g.nextImageID
}

type Image struct {
	graphics *Graphics
	id       graphicsdriver.ImageID
	width    int
	height   int
	screen   bool

	state             _D3D12_RESOURCE_STATES
	texture           *iD3D12Resource1
	layouts           _D3D12_PLACED_SUBRESOURCE_FOOTPRINT
	numRows           uint
	rowSizeInBytes    uint64
	totalBytes        uint64
	stagingBuffer     *iD3D12Resource1
	rtvDescriptorHeap *iD3D12DescriptorHeap
	needsSync         bool
}

func (i *Image) ID() graphicsdriver.ImageID {
	return i.id
}

func (i *Image) Dispose() {
	// Dipose the images later as this image might still be used.
	i.graphics.removeImage(i)
}

func (i *Image) disposeImpl() {
	if i.rtvDescriptorHeap != nil {
		i.rtvDescriptorHeap.Release()
		i.rtvDescriptorHeap = nil
	}
	if i.stagingBuffer != nil {
		i.stagingBuffer.Release()
		i.stagingBuffer = nil
	}
	if i.texture != nil {
		i.texture.Release()
		i.texture = nil
	}
}

func (*Image) IsInvalidated() bool {
	return false
}

func (i *Image) ensureStagingBuffer() error {
	if i.stagingBuffer != nil {
		return nil
	}
	var err error
	i.stagingBuffer, err = createBuffer(i.graphics.device, i.totalBytes)
	if err != nil {
		return err
	}
	return nil
}

func (i *Image) ReadPixels(buf []byte) error {
	if i.screen {
		return errors.New("directx: Pixels cannot be called on the screen")
	}

	if err := i.ensureStagingBuffer(); err != nil {
		return err
	}

	i.transiteState(_D3D12_RESOURCE_STATE_COPY_SOURCE)

	// TODO: Implement this. Probably CopyTextureRegion works?

	return fmt.Errorf("directx: Image.Pixels is not implemented yet")
}

func (i *Image) ReplacePixels(args []*graphicsdriver.ReplacePixelsArgs) error {
	if i.screen {
		return errors.New("directx: ReplacePixels cannot be called on the screen")
	}

	if err := i.ensureStagingBuffer(); err != nil {
		return err
	}

	if i.needsSync {
		// TODO: Read the pixels from GPU
		i.needsSync = false
	}

	i.transiteState(_D3D12_RESOURCE_STATE_COPY_DEST)

	r := _D3D12_RANGE{0, 0}
	m, err := i.stagingBuffer.Map(0, &r)
	if err != nil {
		return err
	}

	var dst []byte
	h := (*reflect.SliceHeader)(unsafe.Pointer(&dst))
	h.Data = uintptr(m)
	h.Len = int(i.totalBytes)
	h.Cap = int(i.totalBytes)
	for _, a := range args {
		for j := 0; j < a.Height; j++ {
			copy(dst[(a.Y+j)*int(i.rowSizeInBytes)+a.X*4:], a.Pixels[j*a.Width*4:(j+1)*a.Width*4])
		}

		dst := _D3D12_TEXTURE_COPY_LOCATION_SubresourceIndex{
			pResource:        i.texture,
			Type:             _D3D12_TEXTURE_COPY_TYPE_SUBRESOURCE_INDEX,
			SubresourceIndex: 0,
		}
		src := _D3D12_TEXTURE_COPY_LOCATION_PlacedFootPrint{
			pResource:       i.stagingBuffer,
			Type:            _D3D12_TEXTURE_COPY_TYPE_PLACED_FOOTPRINT,
			PlacedFootprint: i.layouts,
		}
		i.graphics.commandList.CopyTextureRegion(&dst, uint32(a.X), uint32(a.Y), 0, &src, &_D3D12_BOX{
			left:   uint32(a.X),
			top:    uint32(a.Y),
			front:  0,
			right:  uint32(a.X + a.Width),
			bottom: uint32(a.Y + a.Height),
			back:   1,
		})
	}

	if err := i.stagingBuffer.Unmap(0, nil); err != nil {
		return err
	}

	return nil
}

func (i *Image) resource() *iD3D12Resource1 {
	if i.screen {
		return i.graphics.renderTargets[i.graphics.frameIndex]
	}
	return i.texture
}

func (i *Image) transiteState(newState _D3D12_RESOURCE_STATES) {
	if i.state == newState {
		return
	}

	i.graphics.commandList.ResourceBarrier(1, &_D3D12_RESOURCE_BARRIER_Transition{
		Type:  _D3D12_RESOURCE_BARRIER_TYPE_TRANSITION,
		Flags: _D3D12_RESOURCE_BARRIER_FLAG_NONE,
		Transition: _D3D12_RESOURCE_TRANSITION_BARRIER{
			pResource:   i.resource(),
			Subresource: _D3D12_RESOURCE_BARRIER_ALL_SUBRESOURCES,
			StateBefore: i.state,
			StateAfter:  newState,
		},
	})
	i.state = newState
}

func (i *Image) internalSize() (int, int) {
	if i.screen {
		return i.width, i.height
	}
	return graphics.InternalImageSize(i.width), graphics.InternalImageSize(i.height)
}

func (i *Image) setAsRenderTarget(device *iD3D12Device) error {
	i.transiteState(_D3D12_RESOURCE_STATE_RENDER_TARGET)
	i.needsSync = true

	if i.screen {
		rtv := i.graphics.rtvDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
		rtv.Offset(int32(i.graphics.frameIndex), i.graphics.rtvDescriptorSize)
		i.graphics.commandList.OMSetRenderTargets(1, &rtv, false, nil)
		return nil
	}

	if i.rtvDescriptorHeap == nil {
		h, err := device.CreateDescriptorHeap(&_D3D12_DESCRIPTOR_HEAP_DESC{
			Type:           _D3D12_DESCRIPTOR_HEAP_TYPE_RTV,
			NumDescriptors: 1,
			Flags:          _D3D12_DESCRIPTOR_HEAP_FLAG_NONE,
			NodeMask:       0,
		})
		if err != nil {
			return err
		}
		i.rtvDescriptorHeap = h

		rtv := i.rtvDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
		device.CreateRenderTargetView(i.texture, nil, rtv)
	}

	rtv := i.rtvDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
	i.graphics.commandList.OMSetRenderTargets(1, &rtv, false, nil)

	return nil
}

func copyFloat32s(dst unsafe.Pointer, src []float32) {
	var dsts []float32
	h := (*reflect.SliceHeader)(unsafe.Pointer(&dsts))
	h.Data = uintptr(dst)
	h.Len = len(src)
	h.Cap = len(src)
	copy(dsts, src)
}

func copyUint16s(dst unsafe.Pointer, src []uint16) {
	var dsts []uint16
	h := (*reflect.SliceHeader)(unsafe.Pointer(&dsts))
	h.Data = uintptr(dst)
	h.Len = len(src)
	h.Cap = len(src)
	copy(dsts, src)
}

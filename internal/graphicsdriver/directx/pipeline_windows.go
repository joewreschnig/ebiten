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
	"fmt"
	"math"

	"github.com/hajimehoshi/ebiten/v2/internal/graphics"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver"
)

const numDescriptorsPerFrame = 256

func operationToBlend(c graphicsdriver.Operation) _D3D12_BLEND {
	switch c {
	case graphicsdriver.Zero:
		return _D3D12_BLEND_ZERO
	case graphicsdriver.One:
		return _D3D12_BLEND_ONE
	case graphicsdriver.SrcAlpha:
		return _D3D12_BLEND_SRC_ALPHA
	case graphicsdriver.DstAlpha:
		return _D3D12_BLEND_DEST_ALPHA
	case graphicsdriver.OneMinusSrcAlpha:
		return _D3D12_BLEND_INV_SRC_ALPHA
	case graphicsdriver.OneMinusDstAlpha:
		return _D3D12_BLEND_INV_DEST_ALPHA
	case graphicsdriver.DstColor:
		return _D3D12_BLEND_DEST_COLOR
	default:
		panic(fmt.Sprintf("directx: invalid operation: %d", c))
	}
}

type pipelineStatesKey struct {
	compositeMode graphicsdriver.CompositeMode
	screen        bool
}

type pipelineStatesValue struct {
	rootSignature *iD3D12RootSignature
	vertexShader  *iD3DBlob
	pixelShader   *iD3DBlob
	state         *iD3D12PipelineState
}

type pipelineStates struct {
	cache map[pipelineStatesKey]pipelineStatesValue

	shaderDescriptorHeap *iD3D12DescriptorHeap
	shaderDescriptorSize uint32

	samplerDescriptorHeap *iD3D12DescriptorHeap

	lastFrameIndex  int
	constantBuffers [frameCount][]*iD3D12Resource1
}

const numConstantBufferAndSourceTextures = 1 + graphics.ShaderImageNum

func (p *pipelineStates) initialize(device *iD3D12Device) (ferr error) {
	// Create a CBV/SRV/UAV descriptor heap.
	//   5n+0:        constants
	//   5n+m (1<=4): textures
	shaderH, err := device.CreateDescriptorHeap(&_D3D12_DESCRIPTOR_HEAP_DESC{
		Type:           _D3D12_DESCRIPTOR_HEAP_TYPE_CBV_SRV_UAV,
		NumDescriptors: frameCount * numDescriptorsPerFrame * numConstantBufferAndSourceTextures,
		Flags:          _D3D12_DESCRIPTOR_HEAP_FLAG_SHADER_VISIBLE,
		NodeMask:       0,
	})
	if err != nil {
		return err
	}
	p.shaderDescriptorHeap = shaderH
	defer func() {
		if ferr != nil {
			p.shaderDescriptorHeap.Release()
			p.shaderDescriptorHeap = nil
		}
	}()
	p.shaderDescriptorSize = device.GetDescriptorHandleIncrementSize(_D3D12_DESCRIPTOR_HEAP_TYPE_CBV_SRV_UAV)

	samplerH, err := device.CreateDescriptorHeap(&_D3D12_DESCRIPTOR_HEAP_DESC{
		Type:           _D3D12_DESCRIPTOR_HEAP_TYPE_SAMPLER,
		NumDescriptors: 1,
		Flags:          _D3D12_DESCRIPTOR_HEAP_FLAG_SHADER_VISIBLE,
		NodeMask:       0,
	})
	if err != nil {
		return err
	}
	p.samplerDescriptorHeap = samplerH

	device.CreateSampler(&_D3D12_SAMPLER_DESC{
		Filter:         _D3D12_FILTER_MIN_MAG_MIP_POINT,
		AddressU:       _D3D12_TEXTURE_ADDRESS_MODE_WRAP,
		AddressV:       _D3D12_TEXTURE_ADDRESS_MODE_WRAP,
		AddressW:       _D3D12_TEXTURE_ADDRESS_MODE_WRAP,
		ComparisonFunc: _D3D12_COMPARISON_FUNC_NEVER,
		MinLOD:         -math.MaxFloat32,
		MaxLOD:         math.MaxFloat32,
	}, p.samplerDescriptorHeap.GetCPUDescriptorHandleForHeapStart())

	return nil
}

func (p *pipelineStates) useGraphicsPipelineState(device *iD3D12Device, commandList *iD3D12GraphicsCommandList, frameIndex int, key pipelineStatesKey, screenWidth, screenHeight float32, sourceTexture *iD3D12Resource1) error {
	psv, err := p.graphicsPipelineState(device, key)
	if err != nil {
		return err
	}

	if p.lastFrameIndex != frameIndex {
		p.constantBuffers[frameIndex] = p.constantBuffers[frameIndex][:0]
	}
	p.lastFrameIndex = frameIndex

	idx := len(p.constantBuffers[frameIndex])
	if idx >= numDescriptorsPerFrame*2 {
		return fmt.Errorf("directx: too many constant buffers")
	}

	if cap(p.constantBuffers[frameIndex]) > idx {
		p.constantBuffers[frameIndex] = p.constantBuffers[frameIndex][:idx+1]
	} else {
		p.constantBuffers[frameIndex] = append(p.constantBuffers[frameIndex], nil)
	}

	cb := p.constantBuffers[frameIndex][idx]
	if cb == nil {
		// TODO: What if the buffer size is bigger?
		const bufferSize = 256
		var err error
		cb, err = createBuffer(device, bufferSize)
		if err != nil {
			return err
		}
		p.constantBuffers[frameIndex][idx] = cb

		h := p.shaderDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
		h.Offset(int32(frameIndex*numDescriptorsPerFrame+numConstantBufferAndSourceTextures*idx), p.shaderDescriptorSize)
		device.CreateConstantBufferView(&_D3D12_CONSTANT_BUFFER_VIEW_DESC{
			BufferLocation: cb.GetGPUVirtualAddress(),
			SizeInBytes:    bufferSize,
		}, h)
	}

	// TODO: set multiple textures for custom shaders.
	h := p.shaderDescriptorHeap.GetCPUDescriptorHandleForHeapStart()
	h.Offset(int32(frameIndex*numDescriptorsPerFrame+numConstantBufferAndSourceTextures*idx+1), p.shaderDescriptorSize)
	device.CreateShaderResourceView(sourceTexture, &_D3D12_SHADER_RESOURCE_VIEW_DESC{
		Format:                  _DXGI_FORMAT_R8G8B8A8_UNORM,
		ViewDimension:           _D3D12_SRV_DIMENSION_TEXTURE2D,
		Shader4ComponentMapping: _D3D12_DEFAULT_SHADER_4_COMPONENT_MAPPING,
		Texture2D: _D3D12_TEX2D_SRV{
			MipLevels: 1,
		},
	}, h)

	// Update the constant buffer.
	r := _D3D12_RANGE{0, 0}
	m, err := cb.Map(0, &r)
	if err != nil {
		return err
	}
	copyFloat32s(m, []float32{
		screenWidth,
		screenHeight,
	})
	if err := cb.Unmap(0, nil); err != nil {
		return err
	}

	commandList.SetPipelineState(psv.state)
	commandList.SetGraphicsRootSignature(psv.rootSignature)

	commandList.SetDescriptorHeaps([]*iD3D12DescriptorHeap{
		p.shaderDescriptorHeap,
		p.samplerDescriptorHeap,
	})

	// Match the indices with rootParams in graphicsPipelineState.
	gh := p.shaderDescriptorHeap.GetGPUDescriptorHandleForHeapStart()
	gh.Offset(int32(frameIndex*numDescriptorsPerFrame+numConstantBufferAndSourceTextures*idx), p.shaderDescriptorSize)
	commandList.SetGraphicsRootDescriptorTable(0, gh)

	// TODO: set multiple textures for custom shaders.
	gh.Offset(1, p.shaderDescriptorSize)
	commandList.SetGraphicsRootDescriptorTable(1, gh)

	commandList.SetGraphicsRootDescriptorTable(2, p.samplerDescriptorHeap.GetGPUDescriptorHandleForHeapStart())

	return nil
}

func (p *pipelineStates) graphicsPipelineState(device *iD3D12Device, key pipelineStatesKey) (psv pipelineStatesValue, ferr error) {
	if s, ok := p.cache[key]; ok {
		return s, nil
	}

	if p.cache == nil {
		p.cache = map[pipelineStatesKey]pipelineStatesValue{}
	}

	cbv := _D3D12_DESCRIPTOR_RANGE{
		RangeType:                         _D3D12_DESCRIPTOR_RANGE_TYPE_CBV, // b0
		NumDescriptors:                    1,
		BaseShaderRegister:                0,
		RegisterSpace:                     0,
		OffsetInDescriptorsFromTableStart: _D3D12_DESCRIPTOR_RANGE_OFFSET_APPEND,
	}
	srv := _D3D12_DESCRIPTOR_RANGE{
		RangeType:                         _D3D12_DESCRIPTOR_RANGE_TYPE_SRV, // t0
		NumDescriptors:                    1,
		BaseShaderRegister:                0,
		RegisterSpace:                     0,
		OffsetInDescriptorsFromTableStart: _D3D12_DESCRIPTOR_RANGE_OFFSET_APPEND,
	}
	sampler := _D3D12_DESCRIPTOR_RANGE{
		RangeType:                         _D3D12_DESCRIPTOR_RANGE_TYPE_SAMPLER, // s0
		NumDescriptors:                    1,
		BaseShaderRegister:                0,
		RegisterSpace:                     0,
		OffsetInDescriptorsFromTableStart: _D3D12_DESCRIPTOR_RANGE_OFFSET_APPEND,
	}

	rootParams := [...]_D3D12_ROOT_PARAMETER{
		{
			ParameterType: _D3D12_ROOT_PARAMETER_TYPE_DESCRIPTOR_TABLE,
			DescriptorTable: _D3D12_ROOT_DESCRIPTOR_TABLE{
				NumDescriptorRanges: 1,
				pDescriptorRanges:   &cbv,
			},
			ShaderVisibility: _D3D12_SHADER_VISIBILITY_ALL,
		},
		{
			ParameterType: _D3D12_ROOT_PARAMETER_TYPE_DESCRIPTOR_TABLE,
			DescriptorTable: _D3D12_ROOT_DESCRIPTOR_TABLE{
				NumDescriptorRanges: 1,
				pDescriptorRanges:   &srv,
			},
			ShaderVisibility: _D3D12_SHADER_VISIBILITY_PIXEL,
		},
		{
			ParameterType: _D3D12_ROOT_PARAMETER_TYPE_DESCRIPTOR_TABLE,
			DescriptorTable: _D3D12_ROOT_DESCRIPTOR_TABLE{
				NumDescriptorRanges: 1,
				pDescriptorRanges:   &sampler,
			},
			ShaderVisibility: _D3D12_SHADER_VISIBILITY_PIXEL,
		},
	}

	// Create a root signature.
	sig, err := d3D12SerializeRootSignature(&_D3D12_ROOT_SIGNATURE_DESC{
		NumParameters:     uint32(len(rootParams)),
		pParameters:       &rootParams[0],
		NumStaticSamplers: 0,
		pStaticSamplers:   nil,
		Flags:             _D3D12_ROOT_SIGNATURE_FLAG_ALLOW_INPUT_ASSEMBLER_INPUT_LAYOUT,
	}, _D3D_ROOT_SIGNATURE_VERSION_1_0)
	if err != nil {
		return pipelineStatesValue{}, err
	}
	defer sig.Release()

	rootSignature, err := device.CreateRootSignature(0, sig.GetBufferPointer(), sig.GetBufferSize())
	if err != nil {
		return pipelineStatesValue{}, err
	}
	defer func() {
		if ferr != nil {
			rootSignature.Release()
		}
	}()

	// Create a shader
	shaderSrc := []byte(`struct PSInput {
  float4 position : SV_POSITION;
  float2 texcoord : TEXCOORD0;
  float4 color : COLOR;
};

cbuffer ShaderParameter : register(b0) {
  float2 viewport_size;
}

PSInput VSMain(float2 position : POSITION, float2 tex : TEXCOORD, float4 color : COLOR) {
  // In DirectX, the NDC's Y direction (upward) and the framebuffer's Y direction (downward) don't
  // match. Then, the Y direction must be inverted.
  float4x4 projectionMatrix = {
    2.0 / viewport_size.x, 0, 0, -1,
    0, -2.0 / viewport_size.y, 0, 1,
    0, 0, 1, 0,
    0, 0, 0, 1,
  };

  PSInput result;
  result.position = mul(projectionMatrix, float4(position, 0, 1));
  result.texcoord = tex;
  result.color = float4(color.rgb, 1) * color.a;
  return result;
}

Texture2D tex : register(t0);
SamplerState samp : register(s0);

float4 PSMain(PSInput input) : SV_TARGET {
  return input.color * tex.Sample(samp, input.texcoord);
}`)
	vsh, err := d3DCompile(shaderSrc, "shader", nil, nil, "VSMain", "vs_5_0", 0, 0)
	if err != nil {
		return pipelineStatesValue{}, err
	}
	defer func() {
		if ferr != nil {
			vsh.Release()
		}
	}()

	psh, err := d3DCompile(shaderSrc, "shader", nil, nil, "PSMain", "ps_5_0", 0, 0)
	if err != nil {
		return pipelineStatesValue{}, err
	}
	defer func() {
		if ferr != nil {
			psh.Release()
		}
	}()

	srcOp, dstOp := key.compositeMode.Operations()
	renderTargetBlendDesc := _D3D12_RENDER_TARGET_BLEND_DESC{
		BlendEnable:           1,
		LogicOpEnable:         0,
		SrcBlend:              operationToBlend(srcOp),
		DestBlend:             operationToBlend(dstOp),
		BlendOp:               _D3D12_BLEND_OP_ADD,
		SrcBlendAlpha:         operationToBlend(srcOp),
		DestBlendAlpha:        operationToBlend(dstOp),
		BlendOpAlpha:          _D3D12_BLEND_OP_ADD,
		LogicOp:               _D3D12_LOGIC_OP_NOOP,
		RenderTargetWriteMask: uint8(_D3D12_COLOR_WRITE_ENABLE_ALL),
	}
	rasterizerDesc := _D3D12_RASTERIZER_DESC{
		FillMode:              _D3D12_FILL_MODE_SOLID,
		CullMode:              _D3D12_CULL_MODE_NONE,
		FrontCounterClockwise: 0,
		DepthBias:             _D3D12_DEFAULT_DEPTH_BIAS,
		DepthBiasClamp:        _D3D12_DEFAULT_DEPTH_BIAS_CLAMP,
		SlopeScaledDepthBias:  _D3D12_DEFAULT_SLOPE_SCALED_DEPTH_BIAS,
		DepthClipEnable:       0,
		MultisampleEnable:     0,
		AntialiasedLineEnable: 0,
		ForcedSampleCount:     0,
		ConservativeRaster:    _D3D12_CONSERVATIVE_RASTERIZATION_MODE_OFF,
	}

	// Create a pipeline state.
	psoDesc := _D3D12_GRAPHICS_PIPELINE_STATE_DESC{
		pRootSignature: rootSignature,
		VS: _D3D12_SHADER_BYTECODE{
			pShaderBytecode: vsh.GetBufferPointer(),
			BytecodeLength:  vsh.GetBufferSize(),
		},
		PS: _D3D12_SHADER_BYTECODE{
			pShaderBytecode: psh.GetBufferPointer(),
			BytecodeLength:  psh.GetBufferSize(),
		},
		BlendState: _D3D12_BLEND_DESC{
			AlphaToCoverageEnable:  0,
			IndependentBlendEnable: 0,
			RenderTarget: [8]_D3D12_RENDER_TARGET_BLEND_DESC{
				renderTargetBlendDesc,
			},
		},
		SampleMask:      math.MaxUint32,
		RasterizerState: rasterizerDesc,
		InputLayout: _D3D12_INPUT_LAYOUT_DESC{
			pInputElementDescs: &inputElementDescs[0],
			NumElements:        uint32(len(inputElementDescs)),
		},
		PrimitiveTopologyType: _D3D12_PRIMITIVE_TOPOLOGY_TYPE_TRIANGLE,
		NumRenderTargets:      1,
		RTVFormats: [8]_DXGI_FORMAT{
			_DXGI_FORMAT_R8G8B8A8_UNORM,
		},
		SampleDesc: _DXGI_SAMPLE_DESC{
			Count:   1,
			Quality: 0,
		},
	}

	s, err := device.CreateGraphicsPipelineState(&psoDesc)
	if err != nil {
		return pipelineStatesValue{}, err
	}
	psv = pipelineStatesValue{
		rootSignature: rootSignature,
		vertexShader:  vsh,
		pixelShader:   psh,
		state:         s,
	}
	p.cache[key] = psv
	return psv, nil
}

//go:build amd64

#include "textflag.h"

// func lutGatherSumAVX2(lut *float64, code *byte, nBytes int) float64
// Calcule sum(lut[b*256 + code[b]]) pour b in [0, nBytes), nBytes multiple de 4.
// AVX2 : 4 octets de code par itération — zéro-extension en 4 qwords
// (VPMOVZXBQ), ajout des offsets de position (b*256), VGATHERQPD sur la table,
// accumulation VADDPD. Le masque du gather est reconstruit à chaque itération
// (l'instruction le consomme).
TEXT ·lutGatherSumAVX2(SB), NOSPLIT, $0-32
	MOVQ lut+0(FP), DI
	MOVQ code+8(FP), SI
	MOVQ nBytes+16(FP), CX

	VXORPD Y0, Y0, Y0            // accumulateur A
	VXORPD Y6, Y6, Y6            // accumulateur B (chaîne indépendante)

	// Y1 = offsets de position initiaux [0, 256, 512, 768]
	MOVQ $·lutOffsetsInit(SB), AX
	VMOVDQU (AX), Y1
	// Y2 = incrément par itération de 8 octets [2048 x4]
	MOVQ $·lutOffsetsStep(SB), AX
	VMOVDQU (AX), Y2
	// Y7 = décalage de la seconde chaîne [1024 x4]
	MOVQ $·lutOffsetsHalf(SB), AX
	VMOVDQU (AX), Y7

loop:
	// chaîne A : octets b..b+3
	VPMOVZXBQ (SI), Y3
	VPADDQ Y1, Y3, Y3
	VPCMPEQD Y4, Y4, Y4
	VGATHERQPD Y4, (DI)(Y3*8), Y5
	VADDPD Y5, Y0, Y0
	// chaîne B : octets b+4..b+7 (gather indépendant, latence masquée)
	VPMOVZXBQ 4(SI), Y8
	VPADDQ Y1, Y8, Y8
	VPADDQ Y7, Y8, Y8
	VPCMPEQD Y9, Y9, Y9
	VGATHERQPD Y9, (DI)(Y8*8), Y10
	VADDPD Y10, Y6, Y6
	VPADDQ Y2, Y1, Y1            // offsets += 8*256
	ADDQ $8, SI
	SUBQ $8, CX
	JNZ loop

	// réduction horizontale (Y0+Y6) -> X0
	VADDPD Y6, Y0, Y0
	VEXTRACTF128 $1, Y0, X1
	VADDPD X1, X0, X0
	VHADDPD X0, X0, X0
	MOVSD X0, ret+24(FP)
	VZEROUPPER
	RET

GLOBL ·lutOffsetsInit(SB), RODATA, $32
DATA ·lutOffsetsInit+0(SB)/8, $0
DATA ·lutOffsetsInit+8(SB)/8, $256
DATA ·lutOffsetsInit+16(SB)/8, $512
DATA ·lutOffsetsInit+24(SB)/8, $768

GLOBL ·lutOffsetsStep(SB), RODATA, $32
DATA ·lutOffsetsStep+0(SB)/8, $2048
DATA ·lutOffsetsStep+8(SB)/8, $2048
DATA ·lutOffsetsStep+16(SB)/8, $2048
DATA ·lutOffsetsStep+24(SB)/8, $2048

GLOBL ·lutOffsetsHalf(SB), RODATA, $32
DATA ·lutOffsetsHalf+0(SB)/8, $1024
DATA ·lutOffsetsHalf+8(SB)/8, $1024
DATA ·lutOffsetsHalf+16(SB)/8, $1024
DATA ·lutOffsetsHalf+24(SB)/8, $1024

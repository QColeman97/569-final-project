package main

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"gonum.org/v1/gonum/mat"
)

func matPrint(X mat.Matrix) {
	fa := mat.Formatted(X, mat.Prefix(""), mat.Squeeze())
	fmt.Printf("%v\n", fa)
}

// MPI-FAUN notation:
// X = matrix X, x = vector x
// Xi = ith row block of X, X^i = ith column block of X
// xi = ith row of X, x^i = ith column of X

// Corresponding MPI-FAUN steps in comments
func parallelNMF(node *Node, maxIter int) {
	// Local matrices
	var Wij, Hji mat.Dense

	// 1) Initialize Hji - dims = k x (n/p)
	h := make([]float64, k*smallBlockSizeH)
	for i := range h {
		h[i] = rand.NormFloat64()
	}
	Hji = *mat.NewDense(k, smallBlockSizeH, h)
	// Not in paper, but initialize Wij too - dims = (m/p) x k
	w := make([]float64, smallBlockSizeW*k)
	for i := range w {
		w[i] = rand.NormFloat64()
	}
	Wij = *mat.NewDense(smallBlockSizeW, k, w)

	for iter := 0; iter < maxIter; iter++ {
		// Update W Part
		// 3)
		Uij := &mat.Dense{}
		Uij.Mul(&Hji, Hji.T()) // k x k
		// 4)
		HGramMat := node.allReduce(Uij)
		// 5)
		Hj := node.allGatherAcrossNodeColumns(&Hji) // k x (n/p_c)
		// 6)
		Vij := &mat.Dense{}
		Vij.Mul(node.aPiece, Hj.T()) // (m/pr) x k
		// 7)
		HProductMatij := node.reduceScatterAcrossNodeRows(Vij) // (m/p) x k
		// 8)
		updateW(&Wij, HGramMat, HProductMatij)
		// Update H Part
		// 9)
		Xij := &mat.Dense{}
		Xij.Mul(Wij.T(), &Wij) // k x k
		// 10)
		WGramMat := node.allReduce(Xij)
		// 11)
		Wi := node.allGatherAcrossNodeRows(&Wij) // (m/p_r) x k
		// 12)
		Yij := &mat.Dense{}
		Yij.Mul(Wi.T(), node.aPiece) // k x (n/p_c)
		// 13)
		WProductMatji := node.reduceScatterAcrossNodeColumns(Yij) // k x (n/p)
		// 14)
		updateH(&Hji, WGramMat, WProductMatji)
	}

	// Send Wij & Hji to client
	node.clientChan <- MatMessage{Wij, node.nodeID, true, false}
	node.clientChan <- MatMessage{Hji, node.nodeID, false, true}

	wg.Done()
}

// Line 8 of MPI-FAUN - Multiplicative Update: W = W * ((A @ Ht) / (W @ (H @ Ht)))
// Formula uses: Gram matrix, matrix product w/ A, and W
// 		W dims = (m/p) x k
// 		HGramMat dims = k x k
// 		HProductMatij dims = (m/p) x k
func updateW(W *mat.Dense, HGramMat *mat.Dense, HProductMatij mat.Matrix) {
	update := &mat.Dense{}
	update.Mul(W, HGramMat) // (m/p) x k

	update.DivElem(HProductMatij, update)
	W.MulElem(W, update)
}

// Line 14 of MPI-FAUN - Multiplicative Update: H = H * ((Wt @ A) / ((Wt @ W) @ H))
// Formula uses: Gram matrix, matrix product w/ A, and H
// 		H dims = k x (n/p)
// 		WGramMat dims = k x k
// 		WProductMatji dims = k x (n/p)
func updateH(H *mat.Dense, WGramMat *mat.Dense, WProductMatji mat.Matrix) {
	update := &mat.Dense{}
	update.Mul(WGramMat, H) // k x (n/p)

	update.DivElem(WProductMatji, update)
	H.MulElem(H, update)
}

func partitionAMatrix(A *mat.Dense) []mat.Matrix {
	var piecesOfA []mat.Matrix

	for i := 0; i < numNodeRows; i++ {
		for j := 0; j < numNodeCols; j++ {
			aPiece := A.Slice(largeBlockSizeW*i, largeBlockSizeW*(i+1), largeBlockSizeH*j, largeBlockSizeH*(j+1))
			// Make pieces each their own copies of the data
			piecesOfA = append(piecesOfA, mat.DenseCopyOf(aPiece))
		}
	}

	return piecesOfA
}

func makeNode(chans [numNodes]chan MatMessage, akChans [numNodes]chan bool, clientChan chan MatMessage, id int, aPiece mat.Matrix) *Node {
	return &Node{
		nodeID:     id,
		nodeChans:  chans,
		nodeAks:    akChans,
		inChan:     chans[id],
		aPiece:     aPiece,
		aks:        akChans[id],
		clientChan: clientChan,
	}
}

func makeMatrixChans() [numNodes]chan MatMessage {
	var chans [numNodes]chan MatMessage
	for ch := range chans {
		chans[ch] = make(chan MatMessage, numNodes*3)
	}
	return chans
}

func makeAkChans() [numNodes]chan bool {
	var chans [numNodes]chan bool
	for ch := range chans {
		chans[ch] = make(chan bool, numNodes*3)
	}
	return chans
}

var wg sync.WaitGroup

// Constraints (on m,n,p,p_r,p_c):
// p_r x p_c must = p (grid)
// m / p_r must = p
// n / p_c must = p

//const m, n, k = 16384, 8192, 400
//const numNodes, numNodeRows, numNodeCols = 512, 32, 16

const m, n, k = 2048, 1024, 400
const numNodes, numNodeRows, numNodeCols = 128, 16, 8

const largeBlockSizeW = m / numNodeRows
const largeBlockSizeH = n / numNodeCols
const smallBlockSizeW = m / numNodes
const smallBlockSizeH = n / numNodes

func main() {
	maxIter := 100

	// Initialize input matrix A
	a := make([]float64, m*n)
	for i := 0; i < m*n; i++ {
		a[i] = float64(i) // / 10 // make smaller values, overflow error?
	}
	A := mat.NewDense(m, n, a)
	//aRows, aCols := A.Dims()
	//fmt.Println("A dims:", aRows, aCols)
	//fmt.Println("W dims:", m, k)
	//fmt.Println("H dims:", k, n)
	//fmt.Println("\nA:")
	//matPrint(A)

	// Partition A into pieces for nodes
	piecesOfA := partitionAMatrix(A)
	// Init nodes
	chans := makeMatrixChans()
	akChans := makeAkChans()
	clientChan := make(chan MatMessage, numNodes*3)
	var nodes [numNodes]*Node
	for i := 0; i < numNodes; i++ {
		id := i
		nodes[i] = makeNode(chans, akChans, clientChan, id, piecesOfA[i])
	}

	startTime := time.Now()

	// Launch nodes with their A pieces
	for _, node := range nodes {
		wg.Add(1)
		go parallelNMF(node, maxIter)
	}

	// Wait for W & H blocks from nodes
	wPieces, hPieces := make([]mat.Dense, numNodes), make([]mat.Dense, numNodes)
	for w, h := 0, 0; w < numNodes || h < numNodes; {
		next := <-clientChan
		if next.isFinalW {
			wPieces[next.sentID] = next.mtx
			w++
		} else if next.isFinalH {
			hPieces[next.sentID] = next.mtx
			h++
		}
	}
	wg.Wait()

	// Construct W
	w := make([]float64, m*k)
	for i := 0; i < numNodes; i++ {
		for j := 0; j < smallBlockSizeW; j++ {
			for l := 0; l < k; l++ {
				w[(i*smallBlockSizeW*k)+(j*k)+l] = wPieces[i].At(j, l)
			}
		}
	}
	W := mat.NewDense(m, k, w)

	// Construct H
	h := make([]float64, k*n)
	for j := 0; j < k; j++ {
		for i := 0; i < numNodes; i++ {
			for l := 0; l < smallBlockSizeH; l++ {
				h[(j*numNodes*smallBlockSizeH)+(i*smallBlockSizeH)+l] = hPieces[i].At(j, l)
			}
		}
	}
	H := mat.NewDense(k, n, h)

	// fmt.Println("\nW:")
	// matPrint(W)
	// fmt.Println("\nH:")
	// matPrint(H)

	approxA := &mat.Dense{}
	approxA.Mul(W, H)
	// Truncate values of A to no decimal for ease
	aA := make([]float64, m*n)
	for i := 0; i < m; i++ {
		for j := 0; j < n; j++ {
			aA[(i*n)+j] = math.Round(approxA.At(i, j))
		}
	}
	approxA = mat.NewDense(m, n, aA)
	duration := time.Now().Sub(startTime)
	//fmt.Println("\nApproximation of A:")
	//matPrint(approxA)
	fmt.Println("Took", duration)
}

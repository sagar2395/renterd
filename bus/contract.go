package bus

import (
	"go.sia.tech/renterd/internal/consensus"
	"go.sia.tech/siad/types"
)

// A Contract contains all information about a contract with a host.
type Contract struct {
	ID          types.FileContractID
	HostIP      string `json:"hostIP"`
	HostKey     consensus.PublicKey
	StartHeight uint64 `json:"startHeight"`

	ContractMetadata
}

// ContractMetadata contains all metadata for a contract.
type ContractMetadata struct {
	RenewedFrom types.FileContractID `json:"renewedFrom"`
	Spending    ContractSpending     `json:"spending"`
	TotalCost   types.Currency       `json:"totalCost"`
}

// ContractSpending contains all spending details for a contract.
type ContractSpending struct {
	Uploads     types.Currency `json:"uploads"`
	Downloads   types.Currency `json:"downloads"`
	FundAccount types.Currency `json:"fundAccount"`
}

// Add returns the sum of the current and given contract spending.
func (x ContractSpending) Add(y ContractSpending) (s ContractSpending) {
	s.Uploads = x.Uploads.Add(y.Uploads)
	s.Downloads = x.Downloads.Add(y.Downloads)
	s.FundAccount = x.FundAccount.Add(y.FundAccount)
	return
}
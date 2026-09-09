package qdr

type AdaptorConfig struct {
	Version         int             `json:"version"`
	AddressPrefixes []AddressPrefix `json:"addressPrefixes"`
}

type AddressPrefix struct {
	Prefix string `json:"prefix"`
}

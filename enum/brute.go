// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package enum

import (
	"strings"
	"sync"

	alts "github.com/OWASP/Amass/v3/alterations"
	"github.com/OWASP/Amass/v3/queue"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/stringfilter"
	"github.com/OWASP/Amass/v3/stringset"
)

// BruteManager handles the release of FQDNs generated by brute forcing.
type BruteManager struct {
	sync.Mutex
	enum        *Enumeration
	queue       *queue.Queue
	filter      stringfilter.Filter
	wordlistIdx int
	curReq      *requests.DNSRequest
}

// NewBruteManager returns an initialized BruteManager.
func NewBruteManager(e *Enumeration) *BruteManager {
	return &BruteManager{
		enum:   e,
		queue:  new(queue.Queue),
		filter: stringfilter.NewStringFilter(),
	}
}

// InputName implements the FQDNManager interface.
func (r *BruteManager) InputName(req *requests.DNSRequest) {
	if req == nil || req.Name == "" || req.Domain == "" {
		return
	}

	// Clean up the newly discovered name and domain
	requests.SanitizeDNSRequest(req)

	if !r.enum.Config.IsDomainInScope(req.Name) {
		return
	}

	if !r.enum.Config.BruteForcing || r.filter.Duplicate(req.Name) {
		return
	}

	if len(req.Records) > 0 && (r.enum.hasCNAMERecord(req) || !r.enum.hasARecords(req)) {
		return
	}

	r.queue.Append(req)
}

// OutputNames implements the FQDNManager interface.
func (r *BruteManager) OutputNames(num int) []*requests.DNSRequest {
	r.Lock()
	defer r.Unlock()

	var results []*requests.DNSRequest

	if !r.enum.Config.BruteForcing {
		return results
	}

	var count int
loop:
	for {
		if r.curReq == nil {
			// Get the next subdomain to brute force
			element, ok := r.queue.Next()
			if !ok {
				break loop
			}

			r.curReq = element.(*requests.DNSRequest)
		}

		for {
			// Only return the number of names requested
			if count >= num {
				break loop
			}

			// Check that we haven't used all the words in the list
			if r.wordlistIdx >= len(r.enum.Config.Wordlist) {
				r.curReq = nil
				r.wordlistIdx = 0
				continue loop
			}

			word := r.enum.Config.Wordlist[r.wordlistIdx]
			r.wordlistIdx++
			// Check that we have a good word and generate the new name
			if word != "" {
				count++
				results = append(results, &requests.DNSRequest{
					Name:   word + "." + r.curReq.Name,
					Domain: r.curReq.Domain,
					Tag:    requests.BRUTE,
					Source: "Brute Forcing",
				})
			}
		}
	}

	return results
}

// Stop implements the FQDNManager interface.
func (r *BruteManager) Stop() error {
	r.curReq = nil
	r.wordlistIdx = 0
	r.queue = new(queue.Queue)
	r.filter = stringfilter.NewStringFilter()
	return nil
}

// AlterationsManager handles the release of FQDNs generated by name alterations.
type AlterationsManager struct {
	enum     *Enumeration
	inQueue  *queue.Queue
	outQueue *queue.Queue
	altState *alts.State
}

// NewAlterationsManager returns an initialized AlterationsManager.
func NewAlterationsManager(e *Enumeration) *AlterationsManager {
	am := &AlterationsManager{
		enum:     e,
		inQueue:  new(queue.Queue),
		outQueue: new(queue.Queue),
		altState: alts.NewState(e.Config.AltWordlist),
	}

	am.altState.MinForWordFlip = e.Config.MinForWordFlip
	am.altState.EditDistance = e.Config.EditDistance
	return am
}

// InputName implements the FQDNManager interface.
func (r *AlterationsManager) InputName(req *requests.DNSRequest) {
	if req == nil || req.Name == "" || req.Domain == "" {
		return
	}

	// Clean up the newly discovered name and domain
	requests.SanitizeDNSRequest(req)

	if !r.enum.Config.IsDomainInScope(req.Name) {
		return
	}

	if !r.enum.Config.Alterations {
		return
	}

	if len(strings.Split(req.Name, ".")) <= len(strings.Split(req.Domain, ".")) {
		return
	}

	r.inQueue.Append(req)
}

// OutputNames implements the FQDNManager interface.
func (r *AlterationsManager) OutputNames(num int) []*requests.DNSRequest {
	var results []*requests.DNSRequest

	if !r.enum.Config.Alterations {
		return results
	}

	for num > r.outQueue.Len() {
		if c := r.generateAlts(); c == 0 {
			break
		}
	}

	for i := 0; i < num; i++ {
		element, ok := r.outQueue.Next()
		if !ok {
			break
		}

		results = append(results, element.(*requests.DNSRequest))
	}

	return results
}

// Stop implements the FQDNManager interface.
func (r *AlterationsManager) Stop() error {
	r.inQueue = new(queue.Queue)
	r.outQueue = new(queue.Queue)
	r.altState = alts.NewState(r.enum.Config.AltWordlist)
	return nil
}

func (r *AlterationsManager) generateAlts() int {
	names := stringset.New()

	// Get the next FQDN to generate alterations from
	element, ok := r.inQueue.Next()
	if !ok {
		return 0
	}
	req := element.(*requests.DNSRequest)

	if r.enum.Config.FlipNumbers {
		names.InsertMany(r.altState.FlipNumbers(req.Name)...)
	}
	if r.enum.Config.AddNumbers {
		names.InsertMany(r.altState.AppendNumbers(req.Name)...)
	}
	if r.enum.Config.FlipWords {
		names.InsertMany(r.altState.FlipWords(req.Name)...)
	}
	if r.enum.Config.AddWords {
		names.InsertMany(r.altState.AddSuffixWord(req.Name)...)
		names.InsertMany(r.altState.AddPrefixWord(req.Name)...)
	}
	if r.enum.Config.EditDistance > 0 {
		names.InsertMany(r.altState.FuzzyLabelSearches(req.Name)...)
	}

	var count int
	for name := range names {
		if !r.enum.Config.IsDomainInScope(name) {
			continue
		}

		count++
		r.outQueue.Append(&requests.DNSRequest{
			Name:   name,
			Domain: req.Domain,
			Tag:    requests.ALT,
			Source: "Alterations",
		})
	}

	return count
}

// GuessManager handles the release of FQDNs generated from machine learning.
type GuessManager struct {
	sync.Mutex
	enum         *Enumeration
	markovModel  *alts.MarkovModel
	ttLastOutput int
}

// NewGuessManager returns an initialized GuessManager.
func NewGuessManager(e *Enumeration) *GuessManager {
	return &GuessManager{
		enum:        e,
		markovModel: alts.NewMarkovModel(3),
	}
}

// InputName implements the FQDNManager interface.
func (r *GuessManager) InputName(req *requests.DNSRequest) {
	if req == nil || req.Name == "" || req.Domain == "" {
		return
	}

	// Clean up the newly discovered name and domain
	requests.SanitizeDNSRequest(req)

	if !r.enum.Config.IsDomainInScope(req.Name) {
		return
	}

	if !r.enum.Config.Alterations {
		return
	}

	if len(strings.Split(req.Name, ".")) <= len(strings.Split(req.Domain, ".")) {
		return
	}

	r.markovModel.Train(req.Name)
}

// OutputNames implements the FQDNManager interface.
func (r *GuessManager) OutputNames(num int) []*requests.DNSRequest {
	var results []*requests.DNSRequest

	if !r.enum.Config.Alterations {
		return results
	}

	r.Lock()
	last := r.ttLastOutput
	r.Unlock()

	if r.markovModel.TotalTrainings() < 50 || r.markovModel.TotalTrainings() <= last {
		return results
	}

	r.Lock()
	r.ttLastOutput = r.markovModel.TotalTrainings()
	r.Unlock()

	guesses := stringset.New(r.markovModel.GenerateNames(num * 2)...)
	for num > guesses.Len() {
		guesses.InsertMany(r.markovModel.GenerateNames(num)...)
	}

	var count int
	for name := range guesses {
		if count >= num {
			break
		}

		domain := r.enum.Config.WhichDomain(name)
		if domain == "" || name == "" {
			continue
		}

		count++
		results = append(results, &requests.DNSRequest{
			Name:   name,
			Domain: domain,
			Tag:    requests.GUESS,
			Source: "Markov Model",
		})
	}

	return results
}

// AddSubdomain is unique to the GuessManager and allows newly discovered
// subdomain names to be shared with the MarkovModel object.
func (r *GuessManager) AddSubdomain(sub string) {
	r.markovModel.AddSubdomain(sub)
}

// Stop implements the FQDNManager interface.
func (r *GuessManager) Stop() error {
	r.markovModel = alts.NewMarkovModel(3)
	return nil
}
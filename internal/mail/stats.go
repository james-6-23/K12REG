package mail

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// SlotState is the effective pool state for one virtual address (base or alias).
type SlotState string

const (
	SlotFree         SlotState = "free"
	SlotUsed         SlotState = "used"
	SlotFailed       SlotState = "failed"
	SlotTokenInvalid SlotState = "token_invalid"
	SlotInUse        SlotState = "in_use"
)

// BaseRow summarizes one physical mailbox (base credential line).
type BaseRow struct {
	BaseEmail   string `json:"base_email"`
	AliasCount  int    `json:"alias_count"`
	Free        int    `json:"free"`
	Used        int    `json:"used"`
	Failed      int    `json:"failed"`
	TokenInvalid int   `json:"token_invalid"`
	InUse       int    `json:"in_use"`
	// Status: free | partial | exhausted (any free vs none)
	Status string `json:"status"`
	// BaseKeyState is the state stored on the base email key itself.
	BaseKeyState string `json:"base_key_state"`
}

// AliasSlot is one expanded address.
type AliasSlot struct {
	Address   string    `json:"address"`
	BaseEmail string    `json:"base_email"`
	State     SlotState `json:"state"`
}

// PoolReport is the full mailbox-pool inventory for the control panel.
type PoolReport struct {
	MailboxesFile string `json:"mailboxes_file"`
	StateFile     string `json:"state_file"`
	AliasCount    int    `json:"alias_count"`
	BaseTotal     int    `json:"base_total"`
	SlotTotal     int    `json:"slot_total"`
	Free          int    `json:"free"`
	Used          int    `json:"used"`
	Failed        int    `json:"failed"`
	TokenInvalid  int    `json:"token_invalid"`
	InUse         int    `json:"in_use"`
	Bases         []BaseRow   `json:"bases"`
	Slots         []AliasSlot `json:"slots,omitempty"`
	Error         string      `json:"error,omitempty"`
}

func isDeadState(st string) bool {
	switch st {
	case "used", "failed", "token_invalid", "in_use":
		return true
	default:
		return false
	}
}

func normalizeSlotState(st string) SlotState {
	switch st {
	case "used":
		return SlotUsed
	case "failed":
		return SlotFailed
	case "token_invalid":
		return SlotTokenInvalid
	case "in_use":
		return SlotInUse
	case "":
		return SlotFree
	default:
		// unknown → treat as free so ops can inspect
		if isDeadState(st) {
			return SlotState(st)
		}
		return SlotFree
	}
}

// BuildPoolReport loads the mail file + state and computes free/used alias inventory.
// includeSlots: when true, returns every virtual address (can be large).
func BuildPoolReport(mailboxesFile, statePath string, aliasCount int, includeSlots bool) PoolReport {
	if aliasCount < 1 {
		aliasCount = 1
	}
	rep := PoolReport{
		MailboxesFile: mailboxesFile,
		StateFile:     statePath,
		AliasCount:    aliasCount,
		Bases:         []BaseRow{},
		Slots:         []AliasSlot{},
	}

	data, err := os.ReadFile(mailboxesFile)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	creds := ParseCredentials(string(data))
	if len(creds) == 0 {
		rep.Error = "no credentials in mailboxes file"
		return rep
	}

	// Reuse LoadPool state loading without domain gate.
	p := &Pool{
		items:     expandAliases(creds, aliasCount),
		statePath: statePath,
		state:     map[string]string{},
	}
	p.loadState()

	// Group expanded slots by base.
	type group struct {
		base  string
		slots []Mailbox
	}
	order := make([]string, 0, len(creds))
	byBase := map[string]*group{}
	for _, c := range creds {
		b := strings.ToLower(c.Email)
		if _, ok := byBase[b]; !ok {
			byBase[b] = &group{base: c.Email}
			order = append(order, b)
		}
	}
	for _, mb := range p.items {
		b := strings.ToLower(strings.TrimSpace(mb.BaseEmail))
		if b == "" {
			b = strings.ToLower(mb.Address)
		}
		g := byBase[b]
		if g == nil {
			g = &group{base: mb.BaseEmail}
			byBase[b] = g
			order = append(order, b)
		}
		g.slots = append(g.slots, mb)
	}

	rep.BaseTotal = len(order)
	for _, bk := range order {
		g := byBase[bk]
		if g == nil {
			continue
		}
		row := BaseRow{
			BaseEmail:    g.base,
			AliasCount:   len(g.slots),
			BaseKeyState: p.state[bk],
		}
		// If base key itself is dead, all aliases of this base are unusable (Acquire skips).
		baseDead := isDeadState(p.state[bk])
		for _, mb := range g.slots {
			addr := strings.ToLower(mb.Address)
			st := p.state[addr]
			// Effective: base dead forces used-like exhaustion for free count.
			eff := st
			if baseDead && !isDeadState(st) {
				// Still free in file but Acquire will skip → count as used for "remaining"
				eff = "used"
			}
			ns := normalizeSlotState(eff)
			switch ns {
			case SlotFree:
				row.Free++
				rep.Free++
			case SlotUsed:
				row.Used++
				rep.Used++
			case SlotFailed:
				row.Failed++
				rep.Failed++
			case SlotTokenInvalid:
				row.TokenInvalid++
				rep.TokenInvalid++
			case SlotInUse:
				row.InUse++
				rep.InUse++
			}
			rep.SlotTotal++
			if includeSlots {
				rep.Slots = append(rep.Slots, AliasSlot{
					Address:   mb.Address,
					BaseEmail: mb.BaseEmail,
					State:     ns,
				})
			}
		}
		switch {
		case row.Free == 0:
			row.Status = "exhausted"
		case row.Free == row.AliasCount:
			row.Status = "free"
		default:
			row.Status = "partial"
		}
		rep.Bases = append(rep.Bases, row)
	}

	// Stable sort: free first, then partial, then exhausted; alpha within.
	rank := map[string]int{"free": 0, "partial": 1, "exhausted": 2}
	sort.SliceStable(rep.Bases, func(i, j int) bool {
		ri, rj := rank[rep.Bases[i].Status], rank[rep.Bases[j].Status]
		if ri != rj {
			return ri < rj
		}
		return strings.ToLower(rep.Bases[i].BaseEmail) < strings.ToLower(rep.Bases[j].BaseEmail)
	})
	return rep
}

// ResetBaseState removes state keys for a base email and all of its expanded aliases
// (and any state keys sharing the same local@domain plus-prefix).
func ResetBaseState(statePath string, baseEmail string, mailboxesFile string, aliasCount int) (cleared int, err error) {
	baseEmail = strings.TrimSpace(baseEmail)
	if baseEmail == "" {
		return 0, fmt.Errorf("base email required")
	}
	if aliasCount < 1 {
		aliasCount = 1
	}
	p := &Pool{statePath: statePath, state: map[string]string{}}
	p.loadState()

	baseKey := strings.ToLower(baseEmail)
	keys := map[string]bool{baseKey: true}

	// Expand from file if possible so we clear exact alias addresses.
	if mailboxesFile != "" {
		if data, e := os.ReadFile(mailboxesFile); e == nil {
			creds := ParseCredentials(string(data))
			var match []Credential
			for _, c := range creds {
				if strings.EqualFold(c.Email, baseEmail) {
					match = append(match, c)
				}
			}
			for _, mb := range expandAliases(match, aliasCount) {
				keys[strings.ToLower(mb.Address)] = true
				if b := strings.ToLower(mb.BaseEmail); b != "" {
					keys[b] = true
				}
			}
		}
	}

	// Also clear any plus-aliases already recorded for this local part.
	local, domain, ok := strings.Cut(baseKey, "@")
	local = strings.Split(local, "+")[0]
	if ok {
		for k := range p.state {
			kl := strings.ToLower(k)
			if kl == baseKey {
				keys[kl] = true
				continue
			}
			l, d, ok2 := strings.Cut(kl, "@")
			if !ok2 || d != domain {
				continue
			}
			if strings.Split(l, "+")[0] == local {
				keys[kl] = true
			}
		}
	}

	for k := range keys {
		if _, ok := p.state[k]; ok {
			delete(p.state, k)
			cleared++
		}
	}
	p.saveState()
	return cleared, nil
}

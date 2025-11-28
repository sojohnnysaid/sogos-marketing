package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/mailgun/mailgun-go/v4"
)

// normalizePhone converts phone to E.164 format for Twenty CRM
// Returns empty string if phone can't be normalized
func normalizePhone(phone string) string {
	if phone == "" {
		return ""
	}
	// Strip all non-digits
	re := regexp.MustCompile(`\D`)
	digits := re.ReplaceAllString(phone, "")

	// Need at least 10 digits for US number
	if len(digits) < 10 {
		return ""
	}

	// If 10 digits, assume US and add +1
	if len(digits) == 10 {
		return "+1" + digits
	}

	// If 11 digits starting with 1, add +
	if len(digits) == 11 && digits[0] == '1' {
		return "+" + digits
	}

	// Otherwise return with + prefix
	return "+" + digits
}

type ContactRequest struct {
	Name    string `json:"name"`
	Company string `json:"company"`
	Email   string `json:"email"`
	Phone   string `json:"phone"`
	Message string `json:"message"`
	Service string `json:"service"`
}

type Response struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// Twenty CRM GraphQL types
type GraphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables,omitempty"`
}

type GraphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors,omitempty"`
}

// LeadResult holds the IDs created in Twenty CRM
type LeadResult struct {
	PersonID      string
	CompanyID     string
	OpportunityID string
	IsNewPerson   bool
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/api/contact", corsMiddleware(handleContact))
	http.HandleFunc("/health", handleHealth)

	log.Printf("Server starting on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func handleContact(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ContactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSON(w, http.StatusBadRequest, Response{
			Success: false,
			Message: "Invalid request body",
		})
		return
	}

	// Validate required fields
	if req.Name == "" || req.Email == "" {
		sendJSON(w, http.StatusBadRequest, Response{
			Success: false,
			Message: "Name and email are required",
		})
		return
	}

	// Create lead in Twenty CRM
	var leadResult *LeadResult
	var crmErr error
	leadResult, crmErr = createTwentyLead(req)
	if crmErr != nil {
		log.Printf("Warning: Failed to create Twenty CRM lead: %v", crmErr)
	} else {
		if leadResult.IsNewPerson {
			log.Printf("Created new Twenty CRM lead for %s", req.Email)
		} else {
			log.Printf("Found existing person for %s, created new opportunity", req.Email)
		}
	}

	// Send notification email with CRM link
	if err := sendNotificationEmail(req, leadResult); err != nil {
		log.Printf("Failed to send email: %v", err)
		sendJSON(w, http.StatusInternalServerError, Response{
			Success: false,
			Message: "Failed to send message. Please try again later.",
		})
		return
	}

	sendJSON(w, http.StatusOK, Response{
		Success: true,
		Message: "Thank you for reaching out. We'll be in touch within 24 hours.",
	})
}

func createTwentyLead(req ContactRequest) (*LeadResult, error) {
	apiURL := os.Getenv("TWENTY_API_URL")
	apiKey := os.Getenv("TWENTY_API_KEY")

	if apiURL == "" || apiKey == "" {
		return nil, fmt.Errorf("twenty CRM configuration missing")
	}

	result := &LeadResult{}

	// Parse name into first/last
	nameParts := strings.SplitN(strings.TrimSpace(req.Name), " ", 2)
	firstName := nameParts[0]
	lastName := ""
	if len(nameParts) > 1 {
		lastName = nameParts[1]
	}

	// Step 1: Create or find Company (if provided)
	if req.Company != "" {
		companyID, err := findOrCreateCompany(apiURL, apiKey, req.Company)
		if err != nil {
			log.Printf("Warning: Failed to find/create company: %v", err)
		} else {
			result.CompanyID = companyID
		}
	}

	// Step 2: Find existing person by email or create new one
	personID, isNew, err := findOrCreatePerson(apiURL, apiKey, firstName, lastName, req.Email, req.Phone, result.CompanyID)
	if err != nil {
		return nil, fmt.Errorf("failed to find/create person: %w", err)
	}
	result.PersonID = personID
	result.IsNewPerson = isNew

	// Step 3: Create Opportunity
	opportunityName := fmt.Sprintf("%s - %s", req.Name, req.Service)
	if req.Service == "" {
		opportunityName = fmt.Sprintf("%s - Website Inquiry", req.Name)
	}

	opportunityID, err := createTwentyOpportunity(apiURL, apiKey, opportunityName, req.Message, result.PersonID, result.CompanyID)
	if err != nil {
		return nil, fmt.Errorf("failed to create opportunity: %w", err)
	}
	result.OpportunityID = opportunityID

	return result, nil
}

func findOrCreateCompany(apiURL, apiKey, name string) (string, error) {
	// First, search for existing company by name
	searchQuery := `
		query FindCompany($filter: CompanyFilterInput) {
			companies(filter: $filter) {
				edges {
					node {
						id
						name
					}
				}
			}
		}
	`

	searchVars := map[string]interface{}{
		"filter": map[string]interface{}{
			"name": map[string]interface{}{
				"ilike": "%" + name + "%",
			},
		},
	}

	resp, err := executeTwentyGraphQL(apiURL, apiKey, searchQuery, searchVars)
	if err == nil {
		var searchResult struct {
			Companies struct {
				Edges []struct {
					Node struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"companies"`
		}

		if err := json.Unmarshal(resp.Data, &searchResult); err == nil {
			if len(searchResult.Companies.Edges) > 0 {
				return searchResult.Companies.Edges[0].Node.ID, nil
			}
		}
	}

	// Create new company if not found
	createQuery := `
		mutation CreateCompany($input: CompanyCreateInput!) {
			createCompany(data: $input) {
				id
			}
		}
	`

	createVars := map[string]interface{}{
		"input": map[string]interface{}{
			"name": name,
		},
	}

	resp, err = executeTwentyGraphQL(apiURL, apiKey, createQuery, createVars)
	if err != nil {
		return "", err
	}

	var result struct {
		CreateCompany struct {
			ID string `json:"id"`
		} `json:"createCompany"`
	}

	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return "", fmt.Errorf("failed to parse company response: %w", err)
	}

	return result.CreateCompany.ID, nil
}

func findOrCreatePerson(apiURL, apiKey, firstName, lastName, email, phone, companyID string) (string, bool, error) {
	// Search for existing person by email
	searchQuery := `
		query FindPerson($filter: PersonFilterInput) {
			people(filter: $filter) {
				edges {
					node {
						id
						emails {
							primaryEmail
						}
					}
				}
			}
		}
	`

	searchVars := map[string]interface{}{
		"filter": map[string]interface{}{
			"emails": map[string]interface{}{
				"primaryEmail": map[string]interface{}{
					"ilike": email,
				},
			},
		},
	}

	resp, err := executeTwentyGraphQL(apiURL, apiKey, searchQuery, searchVars)
	if err == nil {
		var searchResult struct {
			People struct {
				Edges []struct {
					Node struct {
						ID     string `json:"id"`
						Emails struct {
							PrimaryEmail string `json:"primaryEmail"`
						} `json:"emails"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"people"`
		}

		if err := json.Unmarshal(resp.Data, &searchResult); err == nil {
			if len(searchResult.People.Edges) > 0 {
				// Found existing person
				return searchResult.People.Edges[0].Node.ID, false, nil
			}
		}
	}

	// Create new person if not found
	createQuery := `
		mutation CreatePerson($input: PersonCreateInput!) {
			createPerson(data: $input) {
				id
			}
		}
	`

	input := map[string]interface{}{
		"name": map[string]interface{}{
			"firstName": firstName,
			"lastName":  lastName,
		},
		"emails": map[string]interface{}{
			"primaryEmail": email,
		},
	}

	// Normalize phone to E.164 format for Twenty CRM
	normalizedPhone := normalizePhone(phone)
	if normalizedPhone != "" {
		input["phones"] = map[string]interface{}{
			"primaryPhoneNumber": normalizedPhone,
		}
	}

	if companyID != "" {
		input["companyId"] = companyID
	}

	createVars := map[string]interface{}{
		"input": input,
	}

	resp, err = executeTwentyGraphQL(apiURL, apiKey, createQuery, createVars)
	if err != nil {
		return "", false, err
	}

	var result struct {
		CreatePerson struct {
			ID string `json:"id"`
		} `json:"createPerson"`
	}

	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return "", false, fmt.Errorf("failed to parse person response: %w", err)
	}

	return result.CreatePerson.ID, true, nil
}

func createTwentyOpportunity(apiURL, apiKey, name, message, personID, companyID string) (string, error) {
	query := `
		mutation CreateOpportunity($input: OpportunityCreateInput!) {
			createOpportunity(data: $input) {
				id
			}
		}
	`

	input := map[string]interface{}{
		"name":  name,
		"stage": "NEW",
	}

	if personID != "" {
		input["pointOfContactId"] = personID
	}

	if companyID != "" {
		input["companyId"] = companyID
	}

	variables := map[string]interface{}{
		"input": input,
	}

	resp, err := executeTwentyGraphQL(apiURL, apiKey, query, variables)
	if err != nil {
		return "", err
	}

	var result struct {
		CreateOpportunity struct {
			ID string `json:"id"`
		} `json:"createOpportunity"`
	}

	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return "", fmt.Errorf("failed to parse opportunity response: %w", err)
	}

	return result.CreateOpportunity.ID, nil
}

func executeTwentyGraphQL(apiURL, apiKey, query string, variables map[string]interface{}) (*GraphQLResponse, error) {
	reqBody := GraphQLRequest{
		Query:     query,
		Variables: variables,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", apiURL+"/graphql", bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, string(body))
	}

	var gqlResp GraphQLResponse
	if err := json.Unmarshal(body, &gqlResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if len(gqlResp.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", gqlResp.Errors[0].Message)
	}

	return &gqlResp, nil
}

func sendNotificationEmail(req ContactRequest, lead *LeadResult) error {
	apiKey := os.Getenv("MAILGUN_API_KEY")
	domain := os.Getenv("MAILGUN_DOMAIN")
	recipient := os.Getenv("CONTACT_EMAIL")
	crmURL := os.Getenv("TWENTY_API_URL")

	if apiKey == "" || domain == "" {
		return fmt.Errorf("mailgun configuration missing")
	}

	if recipient == "" {
		recipient = "john@sogos.io"
	}

	mg := mailgun.NewMailgun(domain, apiKey)

	subject := fmt.Sprintf("🎯 New Lead: %s", req.Name)

	// Build CRM link if we have an opportunity ID
	crmLink := ""
	if lead != nil && lead.OpportunityID != "" {
		crmLink = fmt.Sprintf("\n\n📊 View in CRM: %s/objects/opportunities/%s", crmURL, lead.OpportunityID)
	}

	personStatus := "New contact"
	if lead != nil && !lead.IsNewPerson {
		personStatus = "Existing contact (returning lead)"
	}

	body := fmt.Sprintf(`New lead from sogos.io website!

👤 Contact Information
━━━━━━━━━━━━━━━━━━━━
Name: %s
Company: %s
Email: %s
Phone: %s
Service Interest: %s
Status: %s

💬 Message
━━━━━━━━━━━━━━━━━━━━
%s
%s
`, req.Name, req.Company, req.Email, req.Phone, req.Service, personStatus, req.Message, crmLink)

	m := mg.NewMessage(
		fmt.Sprintf("Sogos CRM <noreply@%s>", domain),
		subject,
		body,
		recipient,
	)

	// Set reply-to as the submitter's email
	m.SetReplyTo(req.Email)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()

	_, _, err := mg.Send(ctx, m)
	return err
}

func sendJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

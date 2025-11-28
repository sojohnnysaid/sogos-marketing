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
	"strings"
	"time"

	"github.com/mailgun/mailgun-go/v4"
)

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

	// Create lead in Twenty CRM (don't fail if this errors)
	if err := createTwentyLead(req); err != nil {
		log.Printf("Warning: Failed to create Twenty CRM lead: %v", err)
	} else {
		log.Printf("Created Twenty CRM lead for %s", req.Email)
	}

	// Send email via Mailgun
	if err := sendContactEmail(req); err != nil {
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

func createTwentyLead(req ContactRequest) error {
	apiURL := os.Getenv("TWENTY_API_URL")
	apiKey := os.Getenv("TWENTY_API_KEY")

	if apiURL == "" || apiKey == "" {
		return fmt.Errorf("twenty CRM configuration missing")
	}

	// Parse name into first/last
	nameParts := strings.SplitN(strings.TrimSpace(req.Name), " ", 2)
	firstName := nameParts[0]
	lastName := ""
	if len(nameParts) > 1 {
		lastName = nameParts[1]
	}

	// Step 1: Create or find Company (if provided)
	var companyID string
	if req.Company != "" {
		var err error
		companyID, err = createTwentyCompany(apiURL, apiKey, req.Company)
		if err != nil {
			log.Printf("Warning: Failed to create company: %v", err)
		}
	}

	// Step 2: Create Person
	personID, err := createTwentyPerson(apiURL, apiKey, firstName, lastName, req.Email, req.Phone, companyID)
	if err != nil {
		return fmt.Errorf("failed to create person: %w", err)
	}

	// Step 3: Create Opportunity
	opportunityName := fmt.Sprintf("%s - %s", req.Name, req.Service)
	if req.Service == "" {
		opportunityName = fmt.Sprintf("%s - Website Inquiry", req.Name)
	}

	err = createTwentyOpportunity(apiURL, apiKey, opportunityName, req.Message, personID, companyID)
	if err != nil {
		return fmt.Errorf("failed to create opportunity: %w", err)
	}

	return nil
}

func createTwentyCompany(apiURL, apiKey, name string) (string, error) {
	query := `
		mutation CreateCompany($input: CompanyCreateInput!) {
			createCompany(data: $input) {
				id
			}
		}
	`

	variables := map[string]interface{}{
		"input": map[string]interface{}{
			"name": name,
		},
	}

	resp, err := executeTwentyGraphQL(apiURL, apiKey, query, variables)
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

func createTwentyPerson(apiURL, apiKey, firstName, lastName, email, phone, companyID string) (string, error) {
	query := `
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

	if phone != "" {
		input["phones"] = map[string]interface{}{
			"primaryPhoneNumber": phone,
		}
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
		CreatePerson struct {
			ID string `json:"id"`
		} `json:"createPerson"`
	}

	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return "", fmt.Errorf("failed to parse person response: %w", err)
	}

	return result.CreatePerson.ID, nil
}

func createTwentyOpportunity(apiURL, apiKey, name, message, personID, companyID string) error {
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

	_, err := executeTwentyGraphQL(apiURL, apiKey, query, variables)
	return err
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

func sendContactEmail(req ContactRequest) error {
	apiKey := os.Getenv("MAILGUN_API_KEY")
	domain := os.Getenv("MAILGUN_DOMAIN")
	recipient := os.Getenv("CONTACT_EMAIL")

	if apiKey == "" || domain == "" {
		return fmt.Errorf("mailgun configuration missing")
	}

	if recipient == "" {
		recipient = "john@sogos.io"
	}

	mg := mailgun.NewMailgun(domain, apiKey)

	subject := fmt.Sprintf("New Contact Request from %s", req.Name)
	body := fmt.Sprintf(`New contact form submission:

Name: %s
Company: %s
Email: %s
Phone: %s
Service Interest: %s

Message:
%s
`, req.Name, req.Company, req.Email, req.Phone, req.Service, req.Message)

	m := mg.NewMessage(
		fmt.Sprintf("Sogos Contact Form <noreply@%s>", domain),
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

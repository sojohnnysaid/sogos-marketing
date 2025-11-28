package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
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

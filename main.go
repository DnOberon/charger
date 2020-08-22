package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stripe/stripe-go/v71"
	"github.com/stripe/stripe-go/v71/client"

	"github.com/dnoberon/charger/airtable"

	"github.com/joho/godotenv"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	airtableAPIKey := os.Getenv("AIRTABLE_API_KEY")
	baseID := os.Getenv("AIRTABLE_BASE_ID")
	tableName := os.Getenv("TABLENAME")

	stripeCustomerIDColumn := os.Getenv("STRIPE_CUSTOMER_ID_COLUMN")
	invoiceAmountColumn := os.Getenv("INVOICE_AMOUNT_COLUMN")
	paidColumn := os.Getenv("PAID_COLUMN")
	notesColumn := os.Getenv("NOTES_COLUMN")
	currencyCodeColumn := os.Getenv("CURRENCY_CODE_COLUMN")

	airtableClient, err := airtable.NewAirtableClient(airtableAPIKey, baseID)
	if err != nil {
		log.Fatal(err)
	}

	// start process loop
	go func() {
		for {
			// fetch all unpaid invoice records
			records, err := airtableClient.ListFromTable(airtable.ListRecordsOptions{
				TableName: tableName,
				Fields: []string{
					stripeCustomerIDColumn,
					invoiceAmountColumn,
					paidColumn,
					currencyCodeColumn,
				},
				FilterByFormula: fmt.Sprintf(`NOT({%s} = 'true')`, paidColumn),
				PageSize:        100, // max records return allowed from airtable
			})

			if err != nil {
				log.Printf("error fetching airtable records %v", err)
				return
			}

			// using records fetch payment methods for customer ID
			for _, record := range records.Records {
				val, ok := record.Fields[stripeCustomerIDColumn]
				if !ok {
					continue
				}

				customerID := fmt.Sprintf("%s", val)

				val, ok = record.Fields[currencyCodeColumn]
				if !ok {
					continue
				}

				currencyCode := fmt.Sprintf("%s", val)

				// invoiceAmount must be a float64
				invoiceAmount, ok := record.Fields[invoiceAmountColumn]
				if !ok {
					continue
				}

				fields := map[string]interface{}{}

				// set paid and notes column for patch update
				confirmationNumber, err := charge(customerID, currencyCode, invoiceAmount.(float64))
				if err != nil {
					fields[notesColumn] = err.Error()
				} else {
					fields[notesColumn] = confirmationNumber
					fields["paid"] = "true"
				}

				// update only the notes and paid columns
				updatedRecord := airtable.Record{ID: record.ID, Fields: fields}
				err = airtableClient.PartialUpdate(airtable.PartialUpdateOptions{TableName: tableName}, updatedRecord)
				if err != nil {
					log.Printf("error updating airtable records %v", err)
					return
				}
			}

			// don't overload the Airtable API
			time.Sleep(time.Second * 1)
		}
	}()

	fmt.Println("Charger Running....")
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc
}

func charge(customerID string, currencyCode string, invoiceAmount float64) (confirmation string, err error) {
	var amount int64

	sc := &client.API{}
	sc.Init(os.Getenv("STRIPE_API_KEY"), nil)

	switch currencyCode {
	case "usd":
		amount = int64(invoiceAmount * 100)
	default:
		return "", errors.New("currency not supported")
	}

	if amount <= 0 {
		return "", errors.New("cannot charge 0 amount")
	}

	i := sc.PaymentMethods.List(&stripe.PaymentMethodListParams{
		Customer: stripe.String(fmt.Sprintf("%v", customerID)),
		Type:     stripe.String("card"),
	})

	if i.Err() != nil {
		return "", i.Err()
	}

	var paid bool

	for i.Next() && !paid {
		paymentID := i.PaymentMethod().ID

		pi, err := sc.PaymentIntents.New(&stripe.PaymentIntentParams{
			Amount:        stripe.Int64(amount),
			Customer:      stripe.String(fmt.Sprintf("%v", customerID)),
			Currency:      stripe.String(fmt.Sprintf("%s", currencyCode)),
			PaymentMethod: stripe.String(paymentID),
		})

		if err != nil {
			return "", err
		}

		confirm, err := sc.PaymentIntents.Confirm(pi.ID, &stripe.PaymentIntentConfirmParams{
			PaymentMethod: stripe.String(paymentID),
		})

		if err != nil {
			return "", err
		}

		return confirm.ID, nil
	}

	return "", errors.New("unable to charge any payment method on file")
}

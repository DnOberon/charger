package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stripe/stripe-go/v71"
	"github.com/stripe/stripe-go/v71/client"

	"github.com/dnoberon/charger/airtable"

	"github.com/joho/godotenv"
)

func main() {
	var staleDays int
	var pollInterval int

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
	dateColumn := os.Getenv("DATE_COLUMN")

	staleDays, err = strconv.Atoi(os.Getenv("STALE_DAYS"))
	if err != nil {
		staleDays = -7
	}

	pollInterval, err = strconv.Atoi(os.Getenv("POLL_INTERVAL"))
	if err != nil {
		pollInterval = 60
	}

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
					dateColumn,
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
				// if any of the required fields are absent, we skip the record. The assumption is that someone is
				// still entering information for that record and we should hold off charging until we have all the
				// information

				// check to see if theres a date, bill only on or after said date
				loc, err := time.LoadLocation(os.Getenv("TIMEZONE"))
				if err != nil {
					log.Fatal("incorrect time location!")
					return
				}

				val, ok := record.Fields[dateColumn]
				if ok {
					date, err := time.ParseInLocation("2006-01-02", fmt.Sprintf("%v", val), loc)
					if err == nil {
						// if we're not on or after date, skip this record
						if !time.Now().In(loc).After(date) {
							log.Printf("date in future, skipping")
							continue
						}

						past := time.Now().In(loc).AddDate(0, 0, staleDays)

						if date.Before(past) {
							log.Printf("pay date too old, skipping")
							continue
						}
					}

				} else if !ok {
					log.Printf("date not present, skipping")
					continue
				}

				// we need to handle rollup fields here, so run reflect and extract if slice
				var customerID string
				val, ok = record.Fields[stripeCustomerIDColumn]
				if !ok {
					log.Printf("customerID not present, skipping")
					continue
				}

				// we're still making some assumptions here - like it's a slice of strings and not ints
				rt := reflect.TypeOf(val)
				switch rt.Kind() {
				case reflect.Slice:
					c := val.([]interface{})
					if len(c) > 0 {
						customerID = fmt.Sprintf("%v", c[0])
					}
				case reflect.Array:
					c := val.([]interface{})
					if len(c) > 0 {
						customerID = fmt.Sprintf("%v", c[0])
					}
				case reflect.String:
					customerID = fmt.Sprintf("%s", val)
				}

				val, ok = record.Fields[currencyCodeColumn]
				if !ok {
					log.Printf("currency code not present, skipping")
					continue
				}

				currencyCode := fmt.Sprintf("%s", val)
				currencyCode = strings.ToLower(currencyCode)

				// invoiceAmount must be a float64
				invoiceAmount, ok := record.Fields[invoiceAmountColumn]
				if !ok {
					log.Printf("invoice amount not present, skipping")
					continue
				}

				fields := map[string]interface{}{}

				// set paid and notes column for patch update
				confirmationNumber, err := charge(customerID, currencyCode, invoiceAmount.(float64))
				if err != nil {
					fields[notesColumn] = fmt.Sprintf("Error charging customer through Stripe: %v", err.Error())
				} else {
					fields[notesColumn] = fmt.Sprintf("Stripe Confirmation Number: %s", confirmationNumber)
					fields[paidColumn] = "true"
				}

				// update only the notes and paid columns
				updatedRecord := airtable.Record{ID: record.ID, Fields: fields}
				err = airtableClient.PartialUpdate(airtable.PartialUpdateOptions{TableName: tableName}, updatedRecord)
				if err != nil {
					log.Printf("error updating airtable records %v", err)
					return
				}

				// don't overload the Airtable API
				time.Sleep(time.Second * 1)
			}
			time.Sleep(time.Duration(pollInterval) * time.Second)
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

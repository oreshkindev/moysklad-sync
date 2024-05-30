package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arcsub/go-moysklad/moysklad"
	"github.com/go-resty/resty/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type (
	Manager struct {
		context    context.Context
		connection *pgxpool.Pool
		client     *moysklad.Client
	}

	Product struct {
		ID                    int
		BrandID               int
		Markup                int
		Depth                 float64
		Height                float64
		Price                 float64
		Weight                float64
		Width                 float64
		Description           string
		Name                  string
		TranslatedDescription string
		TranslatedName        string
		URL                   string
	}

	Currecies[T any] struct {
		Rub T `json:"rub"`
		Try T `json:"try"`
	}

	Currecy struct {
		Code        string  `json:"code"`
		AlphaCode   string  `json:"alphaCode"`
		NumericCode string  `json:"numericCode"`
		Name        string  `json:"name"`
		Rate        float64 `json:"rate"`
		Date        string  `json:"date"`
		InverseRate float64 `json:"inverseRate"`
	}
)

// Get the moysklad access token from the environment variable.
var (
	accessToken string
	mediaFolder string
)

func main() {

	// Create a context that is cancellable and cancel it on exit.
	context, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to the database using the given connection string.
	var (
		connection *pgxpool.Pool
		err        error
	)

	if connection, err = pgxpool.New(context, os.Getenv("DATABASE_URL")); err != nil {
		// If the connection could not be established, panic.
		panic(err)
	}
	// Close the connection when the program exits.
	defer connection.Close()

	if accessToken = os.Getenv("MOYSKLAD_TOKEN"); accessToken == "" {
		// If the access token is not set, panic.
		panic("MOYSKLAD_TOKEN is not set")
	}

	if mediaFolder = os.Getenv("MEDIA_FOLDER"); mediaFolder == "" {
		// If the media folder is not set, panic.
		panic("MEDIA_FOLDER is not set")
	}

	// Run the synchronization loop.
	if err := run(context, connection); err != nil {
		// If the synchronization loop returns an error, print it and exit.
		log.Println(err)
	}
}

// Run runs the main synchronization loop. This loop periodically calls the Sync method
// of the manager. If an error occurs during a Sync call, the error is returned
// immediately.
//
// Args:
//
//	context: A cancellable context that can be used to cancel the loop.
//	connection: A connection pool to the database.
//
// Returns:
//
//	If an error occurs during a Sync call, the error is returned immediately.
//	Otherwise, nil is returned.
func run(context context.Context, connection *pgxpool.Pool) error {

	// The polling interval in minutes.
	const (
		polling = 45
	)

	// Create an instance of the manager.
	manager := New(context, connection)

	// The bucket size defines the interval at which products are collected
	// and synchronized. The value is increased by the bucket value after
	// each iteration.
	if err := manager.Sync(1); err != nil {
		// If the initial sync fails, return the error.
		return err
	}

	// Create a ticker that will fire every 15 minutes.
	ticker := time.NewTicker(polling * time.Minute)

	// Stop the ticker when we're done.
	defer ticker.Stop()

	// Run the loop until the context is cancelled.
	for {
		select {
		case <-ticker.C:
			// Call the Sync method periodically.
			if err := manager.Sync(50); err != nil {
				// If an error occurs, return the error.
				return err
			}
		case <-context.Done():
			// If the context is cancelled, the loop should be terminated.
			return nil
		}
	}
}

// New returns an instance of the manager.
//
// Args:
//
//	context context.Context: A cancellable context that can be used to cancel the
//		synchronization loop.
//	connection *pgxpool.Pool: A connection pool to the database.
//
// Returns:
//
//	*Manager: An instance of the manager.
func New(context context.Context, connection *pgxpool.Pool) *Manager {

	// Create a new client using the access token.
	client := moysklad.NewClient().WithTokenAuth(accessToken)

	// Return a new instance of the manager.
	return &Manager{
		context:    context,
		connection: connection,
		client:     client,
	}
}

// Sync synchronizes the local products with the moysklad service.
//
// Sync runs an infinite loop, collecting products from moysklad, updating
// the local database, and then collecting new sale prices, images, and
// attributes for each product. The collected data is then used to update
// the product in moysklad.
//
// Args:
//
//	interval int: The interval at which products are collected and
//		synchronized. The value is increased by the bucket value after each iteration.
//
// Returns:
//
//	error: An error if something goes wrong during the synchronization process.
func (manager *Manager) Sync(bucket int) error {

	var (
		// The interval at which products are collected and synchronized.
		// The value is increased by the bucket value after each iteration.
		interval int

		// The current value of USD and TRY.
		RUB, TRY *float64

		// An error that may occur during the synchronization process.
		err error
	)

	// The product service used to interact with the moysklad API.
	productService := manager.client.Entity().Product()

	// Collect the current value of USD and TRY.
	if RUB, TRY, err = manager.CollectCurrencies(); err != nil {
		// If an error occurs, stop the synchronization loop and return the error.
		return err
	}

	for {
		var (
			// The collected products.
			collectedProducts []Product

			// The products retrieved from moysklad.
			products []*moysklad.Product

			// An error that may occur during the synchronization process.
			err error
		)

		// Collect products from moysklad.
		if collectedProducts, err = manager.CollectProducts(bucket, interval); err != nil {
			// If an error occurs, stop the synchronization loop and return the error.
			return err
		}

		// If no products are collected, terminate the synchronization loop.
		if len(collectedProducts) == 0 {
			break
		}

		// Iterate over the collected products.
		for _, p := range collectedProducts {

			// Get the product UUID from the local database.
			var (
				productFolder = "25bd3f9b-153d-11ef-0a80-0bf900241c00"
				productUUID   uuid.UUID
				err           error
			)

			if p.Weight == 0.0 || p.Height == 0.0 || p.Width == 0.0 || p.Depth == 0.0 {
				// Set the product folder to the "empty parcer products".
				productFolder = "0634eaef-15e8-11ef-0a80-0e86002b32b8"
			}

			// Check if the product has a UUID.
			if productUUID, err = manager.FindUUID(p.ID); err != nil {
				// If an error occurs, print the error and continue.
				log.Println(err)
			}

			// Create a new moysklad product.
			product := new(moysklad.Product)

			// If the product has a UUID, retrieve the product from moysklad.
			if productUUID != uuid.Nil {
				product.ID = &productUUID

				response, _, err := productService.GetByID(manager.context, &productUUID, nil)
				if err != nil {
					// If an error occurs, print the error and continue.
					log.Println(err)
				}

				// Set the product folder to the "unsorted parser products folder".
				if response != nil {
					product.ProductFolder = response.ProductFolder
				}
			} else {
				// Collect images for the product.
				if productImages, err := manager.CollectImages(p); err != nil {
					// If an error occurs, print the error and continue.
					log.Println(err)
				} else {
					product.Images = productImages

					// Set the product folder to the "unsorted parser products folder".
					product.ProductFolder = manager.ProductFolder(productFolder)
				}
			}

			// Set the product name, description, article, and weight.
			product.Name = moysklad.String(p.Name)

			product.Description = moysklad.String(p.Description)

			product.Article = moysklad.String(strconv.Itoa(p.ID))

			product.Weight = moysklad.Float(p.Weight)

			// Collect sale prices for the product.
			if productSalePrices, err := manager.CollectSalePrices(*RUB, *TRY, p); err != nil {
				// If an error occurs, print the error and continue.
				log.Println(err)
			} else {
				product.SalePrices = productSalePrices
			}

			// Collect attributes for the product.
			if productAttributes, err := manager.CollectAttributes(p); err != nil {
				// If an error occurs, print the error and continue.
				log.Println(err)
			} else {
				product.Attributes = productAttributes
			}

			// Add the product to the list of products to be created in moysklad.
			products = append(products, product)
		}

		// Create the products in moysklad.
		productsCreated, response, err := productService.CreateUpdateMany(manager.context, products, nil)
		if err != nil {
			// print response body
			if err := json.Unmarshal(response.Body(), &response); err != nil {
				log.Println(err)
			}

			fmt.Println(response)
			// If an error occurs, stop the synchronization loop and return the error.
			return err
		}

		// Save the products in the local database.
		if err := manager.SaveProduct(productsCreated); err != nil {
			// If an error occurs, stop the synchronization loop and return the error.
			return err
		}

		// Increase the interval at which products are collected and synchronized.
		interval += bucket
	}

	return nil
}

// FindUUID finds the UUID of a product in the moysklad database.
//
// FindUUID takes an integer product ID and returns a UUID. If the product
// is not found, an error is returned.
//
// Args:
//
//	id int: The product ID to find the UUID for.
//
// Returns:
//
//	uuid.UUID: The UUID of the product, or an error if the product is not found.
func (manager *Manager) FindUUID(id int) (uuid.UUID, error) {

	// Query the moysklad database for the UUID of the product.
	var (
		productUUID uuid.UUID
	)

	query := `
		SELECT id
		FROM moysklad.products
		WHERE product_id = $1;
	`

	if err := manager.connection.QueryRow(manager.context, query, id).Scan(&productUUID); err != nil {
		// Return an error if the product is not found.
		return uuid.Nil, fmt.Errorf("Can't find UUID for product %d: %w", id, err)
	}

	return productUUID, nil
}

// CollectProducts collects products from the database.
//
// CollectProducts returns a slice of Product structs, containing the collected
// products.
//
// Args:
//
//	bucket int: The bucket size defines the interval at which products are collected.
//	interval int: The interval at which products are collected.
//
// Returns:
//
//	[]Product: A slice of Product structs, containing the collected products.
//	error: An error if something goes wrong during the collection process.
func (manager *Manager) CollectProducts(bucket, interval int) ([]Product, error) {

	// Query the database for the products.
	var (
		rows pgx.Rows

		products []Product
		err      error
	)

	query := `
		SELECT id, name, translated_name, url, price, brand_id, markup, weight, depth, height, width, description, translated_description
		FROM provider_product
		LIMIT $1
		OFFSET $2;
	`

	if rows, err = manager.connection.Query(manager.context, query, bucket, interval); err != nil {
		return nil, fmt.Errorf("Can't collect products: %w", err)
	}

	// Scan the rows into a slice of Product structs.
	if products, err = pgx.CollectRows(rows, pgx.RowToStructByName[Product]); err != nil {
		return nil, fmt.Errorf("Can't collect products rows: %w", err)
	}

	return products, nil
}

// FindBrand finds a brand by its ID.
//
// FindBrand takes an ID of a brand and returns its name as a string. If the
// brand is not found, an error is returned.
//
// Args:
//
// brandID int: The ID of the brand to find.
//
// Returns:
//
//	*string: The name of the brand, or an error if the brand is not found.
func (manager *Manager) FindBrand(brandID int) (*string, error) {

	// Execute a query to find the brand name.
	var (
		brand string
	)

	query := `
		SELECT name
		FROM provider_brand
		WHERE id = $1;
	`

	// Scan the result of the query into a variable.
	if err := manager.connection.QueryRow(manager.context, query, brandID).Scan(&brand); err != nil {
		// Return an error if the query fails.
		return nil, fmt.Errorf("Can't find brand: %w", err)
	}

	// Return the brand name if the query succeeds.
	return &brand, nil
}

// CollectCharacteristics collects characteristics of a product from the database.
//
// CollectCharacteristics takes a product ID and returns a string of its
// characteristics. If the product doesn't have any characteristics or
// if the query fails, an error is returned.
//
// Args:
//
// productID int: The ID of the product to find characteristics for.
//
// Returns:
//
//	*string: A string of the product's characteristics, or an error if
//		something goes wrong.
func (manager *Manager) CollectCharacteristics(productID int) (*string, error) {

	// Execute a query to find the characteristics of the product.
	var (
		rows pgx.Rows

		// The slice of characteristics.
		characteristics []string
		err             error
	)

	query := `
        SELECT CONCAT(pc.translated_name, ': ', COALESCE(pcv.translated_value, 'N/A')) AS characteristic
        FROM public.provider_product pp
        LEFT JOIN public.provider_productvalue ppv ON pp.id = ppv.product_id
        LEFT JOIN public.provider_characteristicvalue pcv ON ppv.value_id = pcv.id
        LEFT JOIN public.provider_characteristic pc ON pcv.characteristic_id = pc.id
        WHERE pp.id = $1;
    `

	if rows, err = manager.connection.Query(manager.context, query, productID); err != nil {
		// Return an error if the query fails.
		return nil, fmt.Errorf("Can't find characteristics: %w", err)
	}
	defer rows.Close()

	// Iterate over the rows of the query and append the characteristics to
	// the slice.
	for rows.Next() {
		var (
			// The current characteristic.
			characteristic string
		)

		// Scan the characteristic from the row into the slice.
		if err := rows.Scan(&characteristic); err != nil {
			// Return an error if the scan fails.
			return nil, fmt.Errorf("Can't scan characteristics: %w", err)
		}

		characteristics = append(characteristics, characteristic)
	}

	// Return an error if there is an error when iterating over the rows.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("Can't collect characteristics: %w", err)
	}

	// Join the characteristics into a single string.
	joined := strings.Join(characteristics, ", ")

	return &joined, nil
}

// CollectCurrencies collects the current rates of the US dollar against the Russian
// ruble and Turkish lira from the floatrates.com API.
//
// CollectCurrencies takes no arguments and returns the current rates of the US
// dollar against the Russian ruble and Turkish lira, or an error if something
// goes wrong.
//
// Returns:
//
//	*float64: The current rate of the US dollar against the Russian ruble.
//	*float64: The current rate of the US dollar against the Turkish lira. An
//		error is returned if the API doesn't support the Turkish lira.
//	error: An error if the API request or the unmarshalling fails.
func (manager *Manager) CollectCurrencies() (*float64, *float64, error) {

	// HTTP client to make a GET request to the floatrates.com API.
	http := resty.New()

	// Make a GET request to the floatrates.com API.
	var (
		response *resty.Response

		err error
	)

	if response, err = http.R().Get("https://www.floatrates.com/daily/usd.json"); err != nil {
		// Return an error if the request fails.
		return nil, nil, fmt.Errorf("Can't collect currencies: %w", err)
	}

	if response.IsError() {
		// Return an error if the request returns an error status code.
		return nil, nil, err
	}

	// Unmarshal the response into a Currecies struct.
	var (
		currencies Currecies[Currecy]
	)

	if err := json.Unmarshal(response.Body(), &currencies); err != nil {
		// Return an error if the unmarshalling fails.
		return nil, nil, fmt.Errorf("Can't unmarshal currencies: %w", err)
	}

	// Return the current rates.
	return &currencies.Rub.Rate, &currencies.Try.Rate, nil
}

// SaveProduct saves a slice of moysklad.Product structs to the database.
//
// SaveProduct takes a slice of moysklad.Product structs and returns an error if
// something goes wrong during the saving process.
//
// Args:
//
//	products *[]moysklad.Product: A slice of moysklad.Product structs to be saved
//		to the database.
//
// Returns:
//
//	error: An error if something goes wrong during the saving process.
func (manager *Manager) SaveProduct(products *[]moysklad.Product) error {

	// Start a transaction.
	var (
		tx  pgx.Tx
		err error
	)

	if tx, err = manager.connection.Begin(manager.context); err != nil {
		return fmt.Errorf("Can't start transaction: %w", err)
	}

	// Prepare the query to be executed multiple times.
	query := `
		INSERT INTO moysklad.products (id, product_id)
		VALUES ($1, $2)
		ON CONFLICT (product_id)
		DO UPDATE SET id = $1;
	`

	// Iterate over the products and execute the query for each product.
	for _, product := range *products {
		if _, err := tx.Exec(manager.context, query, product.ID, product.Article); err != nil {
			// Rollback the transaction if the execution fails.
			tx.Rollback(manager.context)
			return err
		}
	}

	// Commit the transaction.
	return tx.Commit(manager.context)
}

// CountLogistic calculates the cost of logistics based on the product's
// parameters and the current exchange rate.
//
// Args:
//
//	weight float64: The weight of the product.
//	depth float64: The depth of the product.
//	height float64: The height of the product.
//	width float64: The width of the product.
//	RUB float64: The current exchange rate in rubles per one USD.
//
// Returns:
//
//	float64: The cost of logistics.
//	error: An error if none.
func (manager *Manager) CountLogistic(weight, depth, height, width, RUB, TRY float64) *float64 {

	var (
		volume, cost float64
	)

	if weight == 0.0 || depth == 0.0 || height == 0.0 || width == 0.0 {
		return nil
	}

	// Calculate the volume of the product.
	volume = depth * width * height / 5000

	// Calculate the cost of logistics.
	cost = RUB * 3

	// If the weight of the product is greater than the volume, add the difference
	// to the cost.
	if weight > volume {
		cost += (weight - volume) * 4.5 * RUB
	} else {
		// Otherwise, add the volume multiplied by the cost per cubic meter to
		// the cost.
		cost += volume * 4.5 * RUB
	}

	return &cost
}

// CollectAttributes collects attributes of the product.
//
// Args:
//
//	product *Product: The product.
//
// Returns:
//
//	*moysklad.Attributes: A slice of moysklad.Attributes.
//	error: An error if none.
func (manager *Manager) CollectAttributes(product Product) (*moysklad.Attributes, error) {

	var (
		brand           *string
		characteristics *string
		err             error
	)

	attributes := new(moysklad.Attributes)

	if product.TranslatedName != "" {
		// Attribute name: переведенное наименование
		attributes.Push(
			manager.Attribute(
				product.TranslatedName,
				"7a15902c-1530-11ef-0a80-15b5002385e8",
			),
		)
	}

	if product.TranslatedDescription != "" {
		// Attribute name: переведенное описание
		attributes.Push(
			manager.Attribute(
				product.TranslatedDescription,
				"7a1592f6-1530-11ef-0a80-15b5002385e9",
			),
		)
	}

	if brand, err = manager.FindBrand(product.BrandID); err != nil {
		return nil, err
	} else {
		// Attribute name: бренд
		attributes.Push(
			manager.Attribute(
				*brand,
				"7a1593f2-1530-11ef-0a80-15b5002385ea",
			),
		)
	}

	if product.URL != "" {
		// Attribute name: ссылка на товар
		attributes.Push(
			manager.Attribute(
				product.URL,
				"0d53d87f-153d-11ef-0a80-04cb0024536b",
			),
		)
	}

	if product.Markup != 0 {
		// Attribute name: коэффициент наценки
		attributes.Push(
			manager.Attribute(
				product.Markup,
				"7a1595cc-1530-11ef-0a80-15b5002385ec",
			),
		)
	}

	if product.Width != 0 {
		// Attribute name: ширина
		attributes.Push(
			manager.Attribute(
				product.Width,
				"7a1596a3-1530-11ef-0a80-15b5002385ed",
			),
		)
	}

	if product.Height != 0 {
		// Attribute name: высота
		attributes.Push(
			manager.Attribute(
				product.Height,
				"7a159855-1530-11ef-0a80-15b5002385ef",
			),
		)
	}

	if product.Depth != 0 {
		// Attribute name: глубина
		attributes.Push(
			manager.Attribute(
				product.Depth,
				"7a159778-1530-11ef-0a80-15b5002385ee",
			),
		)
	}

	if characteristics, err = manager.CollectCharacteristics(product.ID); err != nil {
		return nil, fmt.Errorf("Can't collect characteristics: %w", err)
	} else {
		// Attribute name: характеристики товара
		attributes.Push(
			manager.Attribute(
				*characteristics,
				"7a159956-1530-11ef-0a80-15b5002385f0",
			),
		)
	}

	return attributes, nil
}

// Attribute returns a new moysklad.Attribute with the given value and id.
// The attribute's meta information is set to the correct URL and type.
//
// Args:
//
//	value any: The value of the attribute.
//	id string: The id of the attribute.
//
// Returns:
//
//	*moysklad.Attribute: A new moysklad.Attribute with the given value and id.
func (manager *Manager) Attribute(value any, id string) *moysklad.Attribute {

	return &moysklad.Attribute{
		// meta is the metadata of the attribute.
		Meta: &moysklad.Meta{
			// Href is the URL of the attribute's resource.
			//
			// The URL is constructed based on the id of the attribute.
			Href: moysklad.String("https://api.moysklad.ru/api/remap/1.2/entity/product/metadata/attributes/" + id),
			// Type is the type of the attribute's resource.
			//
			// The type is "attributemetadata".
			Type: "attributemetadata",
		},
		// Value is the value of the attribute.
		Value: value,
	}
}

// CollectSalePrices collects the sale prices of a product.
//
// CollectSalePrices takes the RUB and TRY currency values and the product
// and returns a slice of sale prices. The slice includes the price of the
// parser, the price of ozon with logistics, the price of logistics, the
// price of logistics in USD, the exchange rate of 1$, the exchange rate of
// 1TRY in RUB, and the exchange rate of 1RUB in TRY.
//
// Args:
//
//	RUB	float64: The value of the RUB currency.
//	TRY	float64: The value of the TRY currency.
//	product Product: The product for which the sale prices are collected.
//
// Returns:
//
//	*moysklad.Slice[moysklad.SalePrice]: A slice of sale prices.
func (manager *Manager) CollectSalePrices(RUB, TRY float64, product Product) (*moysklad.Slice[moysklad.SalePrice], error) {

	// Count logistics
	var (
		logistics *float64
	)

	if logistics = manager.CountLogistic(product.Weight, product.Depth, product.Height, product.Width, RUB, TRY); logistics == nil {
		return nil, fmt.Errorf("Can't count logistics. Missing required parameters")
	}

	// Collect sale prices
	salePrices := new(moysklad.Slice[moysklad.SalePrice])

	// ProductType name: Цена парсера (лиры)
	salePrices.Push(manager.SalePrice((product.Price * 100), "TRY", "0c0149ec-153c-11ef-0a80-0daa0026509f"))

	// ProductType name: Цена озон с логистикой (рубли)
	salePrices.Push(manager.SalePrice((((product.Price * (RUB / TRY)) + *logistics) * 100), "RUB", "0c014ae4-153c-11ef-0a80-0daa002650a0"))

	// ProductType name: Цена логистики (рубли)
	salePrices.Push(manager.SalePrice((*logistics * 100), "RUB", "0c014b57-153c-11ef-0a80-0daa002650a1"))

	// ProductType name: Цена логистики ($)
	salePrices.Push(manager.SalePrice(((*logistics / RUB) * 100), "USD", "0c014d0a-153c-11ef-0a80-0daa002650a5"))

	// ProductType name: Курс 1$ (в рубли.)
	salePrices.Push(manager.SalePrice((RUB * 100), "RUB", "0c014bb8-153c-11ef-0a80-0daa002650a2"))

	// ProductType name: Курс 1$ (в лиры)
	salePrices.Push(manager.SalePrice((TRY * 100), "TRY", "0c014c56-153c-11ef-0a80-0daa002650a3"))

	// ProductType name: Курс 1лиры (в рубли)
	salePrices.Push(manager.SalePrice((RUB / TRY * 100), "RUB", "0c014cb1-153c-11ef-0a80-0daa002650a4"))

	return salePrices, nil
}

// SalePrice returns a new moysklad.SalePrice with the given amount,
// currency type and price type ID.
//
// Args:
//
//	amount float64: The value of the sale price.
//	currencyType string: The type of currency of the sale price.
//		priceTypeID string: The ID of the price type of the sale price.
//
// Returns:
//
//	*moysklad.SalePrice: A new moysklad.SalePrice with the given amount,
//							currency type and price type ID.
func (manager *Manager) SalePrice(amount float64, currencyType string, priceTypeID string) *moysklad.SalePrice {

	// Get the ID of the currency based on the currency type.
	var (
		currencyID string
	)

	switch currencyType {
	case "TRY":
		currencyID = "b69c67b9-e09b-11ec-0a80-0ff50002d840"
	case "RUB":
		currencyID = "db85ae09-e09b-11ec-0a80-021300030eb6"
	case "USD":
		currencyID = "7020f17c-6a80-11ed-0a80-06560000034b"
	}

	// Create a new moysklad.SalePrice with the given amount,
	// currency ID and price type ID.
	return &moysklad.SalePrice{
		Value: moysklad.Float(amount),
		Currency: &moysklad.Currency{
			Meta: &moysklad.Meta{
				Href: moysklad.String("https://api.moysklad.ru/api/remap/1.2/entity/currency/" + currencyID),
				// The type of the currency resource.
				//
				// The type is "currency".
				Type: "currency",
			},
		},
		PriceType: &moysklad.PriceType{
			Meta: &moysklad.Meta{
				Href: moysklad.String("https://api.moysklad.ru/api/remap/1.2/context/companysettings/pricetype/" + priceTypeID),
				// The type of the price type resource.
				//
				// The type is "pricetype".
				Type: "pricetype",
			},
		},
	}
}

// CollectImages collects the images of a product.
//
// CollectImages takes the product and returns a slice of moysklad.Image. The
// slice includes the images of the product.
//
// Args:
//
//	Product: The product for which the images are collected.
//
// Returns:
//
//	*moysklad.Images: A slice of moysklad.Image. The slice includes the images
//					 of the product.
func (manager *Manager) CollectImages(product Product) (*moysklad.Images, error) {

	// Try to find the images of the product.
	var (
		productImages moysklad.Images
		href          []string
		err           error
	)

	if href, err = manager.FindImages(product.ID); err != nil || len(href) == 0 {
		return nil, fmt.Errorf("Product %d has no images: %w", product.ID, err)
	}

	// Create a slice of moysklad.Image from the found images.
	for _, h := range href {
		var (
			image *moysklad.Image
			err   error
		)

		if image, err = moysklad.NewImageFromFilepath(mediaFolder + h); err != nil {
			return nil, fmt.Errorf("Can't create image from filepath %s: %w", h, err)
		}

		productImages.Push(image)
	}

	return &productImages, nil
}

// FindImages finds the images of a product from the database.
//
// FindImages takes an integer product ID and returns a slice of image
// filepaths. The slice includes the images of the product.
//
// Args:
//
//	id int: The ID of the product to find images for.
//
// Returns:
//
//	[]string: A slice of image filepaths. The slice includes the images
//				of the product.
func (manager *Manager) FindImages(id int) ([]string, error) {

	// Execute a query to find the images of the product.
	var (
		href []string
	)

	query := `
		SELECT ARRAY_AGG(image)
		FROM provider_productimage
		WHERE product_id = $1
		GROUP BY product_id;
	`

	// Scan the result of the query into a slice of image filepaths.
	if err := manager.connection.QueryRow(manager.context, query, id).Scan(&href); err != nil {
		// Return an error if the query fails.
		return nil, fmt.Errorf("Can't find images: %w", err)
	}

	return href, nil
}

// ProductFolder returns a new moysklad.ProductFolder with the given ID.
//
// ProductFolder takes a string ID of a product folder and returns a
// moysklad.ProductFolder. The new moysklad.ProductFolder is created with the
// given ID.
//
// Args:
//
//	id string: The ID of the product folder to find.
//
// Returns:
//
//	*moysklad.ProductFolder: A new moysklad.ProductFolder with the given ID.
func (manager *Manager) ProductFolder(id string) *moysklad.ProductFolder {

	// Create a new moysklad.ProductFolder with the given ID.
	return &moysklad.ProductFolder{
		Meta: &moysklad.Meta{
			// The href of the product folder resource.
			//
			// The href is the URL of the product folder resource.
			Href: moysklad.String("https://api.moysklad.ru/api/remap/1.2/entity/productfolder/" + id),
			// The type of the product folder resource.
			//
			// The type is "productfolder".
			Type: moysklad.MetaType("productfolder"),
		},
	}
}

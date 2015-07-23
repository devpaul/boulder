// Copyright 2015 ISRG.  All rights reserved
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"bytes"
	"crypto/x509"
	"database/sql"
	"fmt"
	"io/ioutil"
	"strings"
	"text/template"
	"time"

	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/cactus/go-statsd-client/statsd"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/codegangsta/cli"
	"github.com/letsencrypt/boulder/Godeps/_workspace/src/gopkg.in/gorp.v1"

	"github.com/letsencrypt/boulder/cmd"
	"github.com/letsencrypt/boulder/core"
	blog "github.com/letsencrypt/boulder/log"
	"github.com/letsencrypt/boulder/mail"
	"github.com/letsencrypt/boulder/sa"
)

type emailContent struct {
	ExpirationDate   time.Time
	DaysToExpiration int
	CommonName       string
	DNSNames         string
}

const warningTemplate = `Hello,

Your certificate for common name {{.CommonName}} (and DNSNames {{.DNSNames}}) is
going to expire in {{.DaysToExpiration}} days ({{.ExpirationDate}}), make sure you
run the renewer before then!

Regards,
letsencryptbot
`

type mailer struct {
	stats         statsd.Statter
	log           *blog.AuditLogger
	dbMap         *gorp.DbMap
	Mailer        *mail.Mailer
	EmailTemplate *template.Template
	WarningDays   []int
}

func (m *mailer) findExpiringCertificates() error {
	var err error
	now := time.Now()
	// E.g. m.WarningDays = [1, 3, 7, 14] days from expiration
	for i, expiresIn := range m.WarningDays {
		left := now
		if i > 0 {
			left = left.Add(time.Hour * 24 * time.Duration(m.WarningDays[i-1]))
		}
		right := now.Add(time.Hour * 24 * time.Duration(expiresIn))

		m.log.Info(fmt.Sprintf("expiration-mailer: Searching for certificates that expire between %s and %s", left, right))
		var certs []core.Certificate
		_, err := m.dbMap.Select(
			&certs,
			`SELECT cert.* FROM certificates AS cert JOIN certificateStatus AS cs on cs.serial = cert.serial
       WHERE cert.expires > :cutoff-a AND cert.expires < :cutoff-b AND cs.expirationNagsSent < :nags AND cert.status != "revoked"
       ORDER BY cert.expires ASC`,
			map[string]interface{}{
				"cutoff-a": left,
				"cutoff-b": right,
				"nags":     len(m.WarningDays) - i,
			},
		)
		if err == sql.ErrNoRows {
			m.log.Info("expiration-mailer: None found, No expiration emails needed.")
			continue
		} else if err != nil {
			m.log.Err(fmt.Sprintf("expiration-mailer: Error loading certificates: %s", err))
			continue
		}

		m.log.Info(fmt.Sprintf("expiration-mailer: Found %d certificates, starting sending messages", len(certs)))
		for _, cert := range certs {
			regObj, err := m.dbMap.Get(&core.Registration{}, cert.RegistrationID)
			if err != nil {
				return err
			}
			reg := regObj.(core.Registration)
			parsedCert, err := x509.ParseCertificate(cert.DER)
			if err != nil {
				return err
			}
			err = m.sendWarning(parsedCert, reg)
			if err != nil {
				return err
			}

			// Update CertificateStatus object
			tx, err := m.dbMap.Begin()
			if err != nil {
				// BAD
				tx.Rollback()
			}

			csObj, err := tx.Get(&core.CertificateStatus{}, core.SerialToString(parsedCert.SerialNumber))
			if err != nil {
				// BAD
				tx.Rollback()
			}
			certStatus := csObj.(core.CertificateStatus)
			certStatus.ExpirationNagsSent = len(m.WarningDays) - i

			_, err = tx.Update(certStatus)
			if err != nil {
				// BAD
				tx.Rollback()
			}

			err = tx.Commit()
			if err != nil {
				// BAD
				tx.Rollback()
			}
		}
		m.log.Info("expiration-mailer: Finished sending messages")
	}

	return err
}

func (m *mailer) sendWarning(parsedCert *x509.Certificate, reg core.Registration) error {
	emails := []string{}
	for _, contact := range reg.Contact {
		if contact.Scheme == "mailto" {
			emails = append(emails, contact.Opaque)
		}
	}
	if len(emails) > 0 {
		expiresIn := int(time.Now().Sub(parsedCert.NotAfter).Hours() / 24)
		email := emailContent{
			ExpirationDate:   parsedCert.NotAfter,
			DaysToExpiration: expiresIn,
			CommonName:       parsedCert.Subject.CommonName,
			DNSNames:         strings.Join(parsedCert.DNSNames, ", "),
		}
		msgBuf := new(bytes.Buffer)
		err := m.EmailTemplate.Execute(msgBuf, email)
		if err != nil {
			return err
		}
		err = m.Mailer.SendMail(emails, msgBuf.String())
		if err != nil {
			return err
		}
		m.stats.Inc("Mailer.Expiration.Sent", int64(len(emails)), 1.0)
	}
	return nil
}

func main() {
	app := cmd.NewAppShell("expiration-mailer")

	app.App.Flags = append(app.App.Flags, cli.IntFlag{
		Name:   "message_limit",
		EnvVar: "EMAIL_LIMIT",
		Usage:  "Maximum number of emails to send per run",
	})

	app.Config = func(c *cli.Context, config cmd.Config) cmd.Config {
		if c.GlobalInt("emailLimit") > 0 {
			config.Mailer.MessageLimit = c.GlobalInt("emailLimit")
		}
		return config
	}

	app.Action = func(c cmd.Config) {

		// Set up logging
		stats, err := statsd.NewClient(c.Statsd.Server, c.Statsd.Prefix)
		cmd.FailOnError(err, "Couldn't connect to statsd")

		auditlogger, err := blog.Dial(c.Syslog.Network, c.Syslog.Server, c.Syslog.Tag, stats)
		cmd.FailOnError(err, "Could not connect to Syslog")

		// AUDIT[ Error Conditions ] 9cc4d537-8534-4970-8665-4b382abe82f3
		defer auditlogger.AuditPanic()

		blog.SetAuditLogger(auditlogger)

		auditlogger.Info(app.VersionString())

		go cmd.DebugServer(c.Mailer.DebugAddr)

		// Configure DB
		dbMap, err := sa.NewDbMap(c.Mailer.DBDriver, c.Mailer.DBConnect)
		cmd.FailOnError(err, "Could not connect to database")

		// Load email template
		emailTmpl, err := ioutil.ReadFile(c.Mailer.EmailTemplate)
		cmd.FailOnError(err, fmt.Sprintf("Could not read email template file [%s]", c.Mailer.EmailTemplate))
		tmpl, err := template.New("expiry-email").Parse(string(emailTmpl))
		cmd.FailOnError(err, "Could not parse email template")

		mailClient := mail.NewMailer(c.Mailer.Server, c.Mailer.Port, c.Mailer.Username, c.Mailer.Password)

		m := mailer{
			stats:         stats,
			log:           auditlogger,
			dbMap:         dbMap,
			Mailer:        &mailClient,
			EmailTemplate: tmpl,
			WarningDays:   c.Mailer.ExpiryWarnings,
		}

		auditlogger.Info("expiration-mailer: Starting")
		err = m.findExpiringCertificates()
		cmd.FailOnError(err, "Could not connect to database")
	}

	app.Run()
}
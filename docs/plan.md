# Tommy - universal testing tool

## Idea

There is a popular project - https://mailcatcher.me/

It pretends to be a mail server and allows to test emails without using real email services which is super helpful.

Tommy should be the same and much more. It should provide a quick and nice way to mock different services that are hard to test locally:

* emails
* sms messages
* push notifications
* ftp uploads

In all these cases it would be nice to have a local service that allows to easily inspect the content of the information that was sent
and use it for testing purposes.

## General requirements

* The build result should be a single binary, no dependencies
* tommy should provide the ui on a configurable port
* UI should be simple, but extensible (tabs), for every type of the payload there should be possible to have a separate UI
* tommy should attach metadata to the uploaded assets wherever thay make sense (email sent via mailjet API)
* tommy should provide an api on a configurable port that would allow to inspect the incoming information (new emails and their content, new sms messages and their content etc), event format should be defined per content type
* it should be possible to run one content type from the cli with minimal configuration (e.g. `tommy mail --ui-port 8811 --in-port 8822 --enabled providers mailjet,sendgrid`)
* it should be possible to run tommy against a toml config with all the configuration required and possibility to run multiple providers at once
* the project should be covered with the tests both for API and UI
* the different plugins (mail, ftp) should be implemented as independently as possible to allow for concurrent implementation. Same thing for extensions within a plugin (concurrently implement the support for mailjet and sendgrid providers within mail plugin)

## Per Plugin requirements

Let's start with two plugins and outline the rest later

### Mail

Let's start with imitating two providers:

* Mailjet: https://dev.mailjet.com/email/guides/send-api-v31/
* Sendgrid: https://www.twilio.com/docs/sendgrid/api-reference/mail-send/mail-send

Only actual emails sends (headers + body) should be supported, dynamic templates are out of scope. Attachments should be supported

###  SMS

Only one provider should be supported for now:

* Twilio: https://www.twilio.com/docs/messaging/api

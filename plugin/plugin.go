package plugin

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	gorm "github.com/infobloxopen/protoc-gen-gorm/options"
	jgorm "github.com/jinzhu/gorm"
	"github.com/jinzhu/inflection"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

var (
	ErrNotOrmable = errors.New("type is not ormable")
)

var (
	uuidImport         = "github.com/satori/go.uuid"
	gtypesImport       = "github.com/infobloxopen/protoc-gen-gorm/types"
	ocTraceImport      = "go.opencensus.io/trace"
	pqImport           = "github.com/lib/pq"
	gerrorsImport      = "github.com/infobloxopen/protoc-gen-gorm/errors"
	timestampImport    = "google.golang.org/protobuf/types/known/timestamppb"
	wktImport          = "google.golang.org/protobuf/types/known/wrapperspb"
	fmImport           = "google.golang.org/genproto/protobuf/field_mask"
	resourceImport     = "github.com/infobloxopen/atlas-app-toolkit/gorm/resource"
	gormpqImport       = "github.com/jinzhu/gorm/dialects/postgres"
	stdFmtImport       = "fmt"
	stdCtxImport       = "context"
	stdStringsImport   = "strings"
	stdTimeImport      = "time"
	encodingJsonImport = "encoding/json"
	bigintImport       = "math/big"
)

var builtinTypes = map[string]struct{}{
	"bool":    {},
	"int":     {},
	"int8":    {},
	"int16":   {},
	"int32":   {},
	"int64":   {},
	"uint":    {},
	"uint8":   {},
	"uint16":  {},
	"uint32":  {},
	"uint64":  {},
	"uintptr": {},
	"float32": {},
	"float64": {},
	"string":  {},
	"[]byte":  {},
}

var wellKnownTypes = map[string]string{
	"StringValue": "*string",
	"DoubleValue": "*float64",
	"FloatValue":  "*float32",
	"Int32Value":  "*int32",
	"Int64Value":  "*int64",
	"UInt32Value": "*uint32",
	"UInt64Value": "*uint64",
	"BoolValue":   "*bool",
	//  "BytesValue" : "*[]byte",
}

const (
	protoTypeTimestamp = "Timestamp" // last segment, first will be *google_protobufX
	protoTypeJSON      = "JSONValue"
	protoTypeUUID      = "UUID"
	protoTypeUUIDValue = "UUIDValue"
	protoTypeResource  = "Identifier"
	protoTypeInet      = "InetValue"
	protoTimeOnly      = "TimeOnly"
	protoTypeBigInt    = "BigInt"
)

// DB Engine Enum
const (
	ENGINE_UNSET = iota
	ENGINE_POSTGRES
)

type ORMBuilder struct {
	plugin         *protogen.Plugin
	ormableTypes   map[string]*OrmableType
	messages       map[string]struct{}
	currentFile    string
	currentPackage string
	dbEngine       int
	stringEnums    bool
	suppressWarn   bool
}

func New(opts protogen.Options, request *pluginpb.CodeGeneratorRequest) (*ORMBuilder, error) {
	plugin, err := opts.New(request)
	if err != nil {
		return nil, err
	}

	builder := &ORMBuilder{
		plugin:       plugin,
		ormableTypes: make(map[string]*OrmableType),
		messages:     make(map[string]struct{}),
	}

	params := parseParameter(request.GetParameter())

	if strings.EqualFold(params["engine"], "postgres") {
		builder.dbEngine = ENGINE_POSTGRES
	} else {
		builder.dbEngine = ENGINE_UNSET
	}

	if strings.EqualFold(params["enums"], "string") {
		builder.stringEnums = true
	}

	if _, ok := params["quiet"]; ok {
		builder.suppressWarn = true
	}

	return builder, nil
}

func parseParameter(param string) map[string]string {
	paramMap := make(map[string]string)

	params := strings.Split(param, ",")
	for _, param := range params {
		if strings.Contains(param, "=") {
			kv := strings.Split(param, "=")
			paramMap[kv[0]] = kv[1]
			continue
		}
		paramMap[param] = ""
	}

	return paramMap
}

type OrmableType struct {
	File       *protogen.File
	Fields     map[string]*Field
	Methods    map[string]*autogenMethod
	Name       string
	OriginName string
	Package    string
}

func NewOrmableType(originalName string, pkg string, file *protogen.File) *OrmableType {
	return &OrmableType{
		OriginName: originalName,
		Package:    pkg,
		File:       file,
		Fields:     make(map[string]*Field),
		Methods:    make(map[string]*autogenMethod),
	}
}

type Field struct {
	*gorm.GormFieldOptions
	ParentGoType   string
	Type           string
	Package        string
	ParentOrigName string
}

type autogenMethod struct {
	*protogen.Method
	outType           *protogen.Message
	inType            *protogen.Message
	verb              string
	baseType          string
	fieldMaskName     string
	ccName            string
	followsConvention bool
}

type fileImports struct {
	wktPkgName      string
	packages        map[string]*pkgImport
	typesToRegister []string
	stdImports      []string
}

type pkgImport struct {
	packagePath string
	alias       string
}

func (b *ORMBuilder) Generate() (*pluginpb.CodeGeneratorResponse, error) {
	genFileMap := make(map[string]*protogen.GeneratedFile)

	for _, protoFile := range b.plugin.Files {
		fileName := protoFile.GeneratedFilenamePrefix + ".pb.gorm.go"
		g := b.plugin.NewGeneratedFile(fileName, ".")
		genFileMap[fileName] = g

		b.currentPackage = protoFile.GoImportPath.String()

		// first traverse: preload the messages
		for _, message := range protoFile.Messages {
			if message.Desc.IsMapEntry() {
				continue
			}

			typeName := string(message.Desc.Name())
			b.messages[typeName] = struct{}{}

			if isOrmable(message) {
				ormable := NewOrmableType(typeName, string(protoFile.GoPackageName), protoFile)
				b.ormableTypes[typeName] = ormable
			}
		}

		// second traverse: parse basic fields
		for _, message := range protoFile.Messages {
			if isOrmable(message) {
				b.parseBasicFields(message, g)
			}
		}

		// third traverse: build associations
		for _, message := range protoFile.Messages {
			typeName := string(message.Desc.Name())
			if isOrmable(message) {
				b.parseAssociations(message, g)
				o := b.getOrmable(typeName)
				if b.hasPrimaryKey(o) {
					_, fd := b.findPrimaryKey(o)
					fd.ParentOrigName = o.OriginName
				}
			}
		}

	}

	for _, protoFile := range b.plugin.Files {
		// generate actual code
		fileName := protoFile.GeneratedFilenamePrefix + ".pb.gorm.go"
		g, ok := genFileMap[fileName]
		if !ok {
			panic("generated file should be present")
		}

		if !protoFile.Generate {
			g.Skip()
			continue
		}

		skip := true

		for _, message := range protoFile.Messages {
			if isOrmable(message) {
				skip = false
				break
			}
		}

		if skip {
			g.Skip()
			continue
		}

		g.P("package ", protoFile.GoPackageName)

		for _, message := range protoFile.Messages {
			if isOrmable(message) {
				b.generateOrmable(g, message)
				b.generateTableNameFunctions(g, message)
				b.generateConvertFunctions(g, message)
				b.generateHookInterfaces(g, message)
			}
		}
	}

	return b.plugin.Response(), nil
}

func (b *ORMBuilder) generateConvertFunctions(g *protogen.GeneratedFile, message *protogen.Message) {
	typeName := string(message.Desc.Name())
	ormable := b.getOrmable(camelCase(typeName))

	///// To Orm
	g.P(`// ToORM runs the BeforeToORM hook if present, converts the fields of this`)
	g.P(`// object to ORM format, runs the AfterToORM hook, then returns the ORM object`)
	g.P(`func (m *`, typeName, `) ToORM (ctx `, generateImport("Context", "context", g), `) (`, typeName, `ORM, error) {`)
	g.P(`to := `, typeName, `ORM{}`)
	g.P(`var err error`)
	g.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToORM); ok {`)
	g.P(`if err = prehook.BeforeToORM(ctx, &to); err != nil {`)
	g.P(`return to, err`)
	g.P(`}`)
	g.P(`}`)
	for _, field := range message.Fields {
		// Checking if field is skipped
		options := field.Desc.Options().(*descriptorpb.FieldOptions)
		fieldOpts := getFieldOptions(options)
		if fieldOpts.GetDrop() {
			continue
		}

		ofield := ormable.Fields[camelCase(field.GoName)]
		b.generateFieldConversion(message, field, true, ofield, g)
	}
	// if getMessageOptions(message).GetMultiAccount() {
	// 	g.P("accountID, err := ", generateImport("GetAccountID", authImport, g), "(ctx, nil)")
	// 	g.P("if err != nil {")
	// 	g.P("return to, err")
	// 	g.P("}")
	// 	g.P("to.AccountID = accountID")
	// }
	b.setupOrderedHasMany(message, g)
	g.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToORM); ok {`)
	g.P(`err = posthook.AfterToORM(ctx, &to)`)
	g.P(`}`)
	g.P(`return to, err`)
	g.P(`}`)

	g.P()
	///// To Pb
	g.P(`// ToPB runs the BeforeToPB hook if present, converts the fields of this`)
	g.P(`// object to PB format, runs the AfterToPB hook, then returns the PB object`)
	g.P(`func (m *`, typeName, `ORM) ToPB (ctx context.Context) (`,
		typeName, `, error) {`)
	g.P(`to := `, typeName, `{}`)
	g.P(`var err error`)
	g.P(`if prehook, ok := interface{}(m).(`, typeName, `WithBeforeToPB); ok {`)
	g.P(`if err = prehook.BeforeToPB(ctx, &to); err != nil {`)
	g.P(`return to, err`)
	g.P(`}`)
	g.P(`}`)
	for _, field := range message.Fields {
		// Checking if field is skipped
		options := field.Desc.Options().(*descriptorpb.FieldOptions)
		fieldOpts := getFieldOptions(options)
		if fieldOpts.GetDrop() {
			continue
		}
		ofield := ormable.Fields[camelCase(field.GoName)]
		b.generateFieldConversion(message, field, false, ofield, g)
	}
	g.P(`if posthook, ok := interface{}(m).(`, typeName, `WithAfterToPB); ok {`)
	g.P(`err = posthook.AfterToPB(ctx, &to)`)
	g.P(`}`)
	g.P(`return to, err`)
	g.P(`}`)
}

func (b *ORMBuilder) generateTableNameFunctions(g *protogen.GeneratedFile, message *protogen.Message) {
	typeName := string(message.Desc.Name())
	msgName := string(message.Desc.Name())

	g.P(`// TableName overrides the default tablename generated by GORM`)
	g.P(`func (`, typeName, `ORM) TableName() string {`)

	tableName := inflection.Plural(jgorm.ToDBName(msgName))
	if opts := getMessageOptions(message); opts != nil && len(opts.Table) > 0 {
		tableName = opts.GetTable()
	}
	g.P(`return "`, tableName, `"`)
	g.P(`}`)
}

func (b *ORMBuilder) generateOrmable(g *protogen.GeneratedFile, message *protogen.Message) {
	ormable := b.getOrmable(message.GoIdent.GoName)
	g.P(`type `, ormable.Name, ` struct {`)

	var names []string
	for name := range ormable.Fields {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		field := ormable.Fields[name]
		sp := strings.Split(field.Type, ".")

		if len(sp) == 2 && sp[1] == "UUID" {
			s := generateImport("UUID", uuidImport, g)
			if field.Type[0] == '*' {
				field.Type = "*" + s
			} else {
				field.Type = s
			}
		}

		if len(sp) == 2 && sp[1] == "BigInt" {
			s := generateImport("BigInt", bigintImport, g)
			if field.Type[0] == '*' {
				field.Type = "*" + s
			} else {
				field.Type = s
			}
		}

		g.P(name, ` `, field.Type, b.renderGormTag(field))
	}

	g.P(`}`)
	g.P()
}

func (b *ORMBuilder) parseAssociations(msg *protogen.Message, g *protogen.GeneratedFile) {
	typeName := camelCase(string(msg.Desc.Name())) // TODO: camelSnakeCase
	ormable := b.getOrmable(typeName)

	for _, field := range msg.Fields {
		options := field.Desc.Options().(*descriptorpb.FieldOptions)
		fieldOpts := getFieldOptions(options)
		if fieldOpts.GetDrop() {
			continue
		}

		fieldName := camelCase(string(field.Desc.Name()))
		var fieldType string

		if field.Desc.Message() == nil {
			fieldType = field.Desc.Kind().String() // was GoType
		} else {
			fieldType = string(field.Desc.Message().Name())
		}

		fieldType = strings.Trim(fieldType, "[]*")
		parts := strings.Split(fieldType, ".")
		fieldTypeShort := parts[len(parts)-1]

		if b.isOrmable(fieldType) {
			if fieldOpts == nil {
				fieldOpts = &gorm.GormFieldOptions{}
			}
			assocOrmable := b.getOrmable(fieldType)

			if field.Message != nil {
				fieldType = b.typeName(field.Message.GoIdent, g)
			}

			if field.Desc.Cardinality() == protoreflect.Repeated {
				if fieldOpts.GetManyToMany() != nil {
					b.parseManyToMany(msg, ormable, fieldName, fieldTypeShort, assocOrmable, fieldOpts)
				} else {
					b.parseHasMany(msg, ormable, fieldName, fieldTypeShort, assocOrmable, fieldOpts)
				}
				fieldType = fmt.Sprintf("[]*%sORM", fieldType)
			} else {
				if fieldOpts.GetBelongsTo() != nil {
					b.parseBelongsTo(msg, ormable, fieldName, fieldTypeShort, assocOrmable, fieldOpts)
				} else {
					b.parseHasOne(msg, ormable, fieldName, fieldTypeShort, assocOrmable, fieldOpts)
				}
				fieldType = fmt.Sprintf("*%sORM", fieldType)
			}

			// Register type used, in case it's an imported type from another package
			// b.GetFileImports().typesToRegister = append(b.GetFileImports().typesToRegister, fieldType) // maybe we need other fields type
			ormable.Fields[fieldName] = &Field{Type: fieldType, GormFieldOptions: fieldOpts}
		}
	}
}

func (b *ORMBuilder) hasPrimaryKey(ormable *OrmableType) bool {
	for _, field := range ormable.Fields {
		if field.GetTag().GetPrimaryKey() {
			return true
		}
	}
	for fieldName := range ormable.Fields {
		if strings.ToLower(fieldName) == "id" {
			return true
		}
	}
	return false
}

func (b *ORMBuilder) isOrmable(typeName string) bool {
	_, ok := b.ormableTypes[typeName]
	return ok
}

func (b *ORMBuilder) findPrimaryKey(ormable *OrmableType) (string, *Field) {
	for fieldName, field := range ormable.Fields {
		if field.GetTag().GetPrimaryKey() {
			return fieldName, field
		}
	}
	for fieldName, field := range ormable.Fields {
		if strings.ToLower(fieldName) == "id" {
			return fieldName, field
		}
	}

	panic("no primary_key")
}

func (b *ORMBuilder) getOrmable(typeName string) *OrmableType {
	r, err := GetOrmable(b.ormableTypes, typeName)
	if err != nil {
		panic(err)
	}

	return r
}

func (b *ORMBuilder) parseManyToMany(msg *protogen.Message, ormable *OrmableType, fieldName string, fieldType string, assoc *OrmableType, opts *gorm.GormFieldOptions) {
	typeName := camelCase(string(msg.Desc.Name()))
	mtm := opts.GetManyToMany()
	if mtm == nil {
		mtm = &gorm.ManyToManyOptions{}
		opts.Association = &gorm.GormFieldOptions_ManyToMany{mtm}
	}

	var foreignKeyName string
	if foreignKeyName = camelCase(mtm.GetForeignkey()); foreignKeyName == "" {
		foreignKeyName, _ = b.findPrimaryKey(ormable)
	} else {
		var ok bool
		_, ok = ormable.Fields[foreignKeyName]
		if !ok {
			panic(fmt.Sprintf("Missing %s field in %s", foreignKeyName, ormable.Name))
		}
	}
	mtm.Foreignkey = foreignKeyName
	var assocKeyName string
	if assocKeyName = camelCase(mtm.GetAssociationForeignkey()); assocKeyName == "" {
		assocKeyName, _ = b.findPrimaryKey(assoc)
	} else {
		var ok bool
		_, ok = assoc.Fields[assocKeyName]
		if !ok {
			panic(fmt.Sprintf("Missing %s field in %s", assocKeyName, assoc.Name))
		}
	}
	mtm.AssociationForeignkey = assocKeyName
	var jt string
	if jt = jgorm.ToDBName(mtm.GetJointable()); jt == "" {
		if b.countManyToManyAssociationDimension(msg, fieldType) == 1 && typeName != fieldType {
			jt = jgorm.ToDBName(typeName + inflection.Plural(fieldType))
		} else {
			jt = jgorm.ToDBName(typeName + inflection.Plural(fieldName))
		}
	}
	mtm.Jointable = jt
	var jtForeignKey string
	if jtForeignKey = camelCase(mtm.GetJointableForeignkey()); jtForeignKey == "" {
		jtForeignKey = camelCase(jgorm.ToDBName(typeName + foreignKeyName))
	}
	mtm.JointableForeignkey = jtForeignKey
	var jtAssocForeignKey string
	if jtAssocForeignKey = camelCase(mtm.GetAssociationJointableForeignkey()); jtAssocForeignKey == "" {
		if typeName == fieldType {
			jtAssocForeignKey = jgorm.ToDBName(inflection.Singular(fieldName) + assocKeyName)
		} else {
			jtAssocForeignKey = jgorm.ToDBName(fieldType + assocKeyName)
		}
	}
	mtm.AssociationJointableForeignkey = camelCase(jtAssocForeignKey)
}

func (b *ORMBuilder) parseHasOne(msg *protogen.Message, parent *OrmableType, fieldName string, fieldType string, child *OrmableType, opts *gorm.GormFieldOptions) {
	typeName := camelCase(string(msg.Desc.Name()))
	hasOne := opts.GetHasOne()
	if hasOne == nil {
		hasOne = &gorm.HasOneOptions{}
		opts.Association = &gorm.GormFieldOptions_HasOne{hasOne}
	}

	var assocKey *Field
	var assocKeyName string

	if assocKeyName = camelCase(hasOne.GetAssociationForeignkey()); assocKeyName == "" {
		assocKeyName, assocKey = b.findPrimaryKey(parent)
	} else {
		var ok bool
		assocKey, ok = parent.Fields[assocKeyName]
		if !ok {
			panic(fmt.Sprintf("Missing %s field in %s.", assocKeyName, parent.Name))
		}
	}

	hasOne.AssociationForeignkey = assocKeyName
	var foreignKeyType string
	if hasOne.GetForeignkeyTag().GetNotNull() {
		foreignKeyType = strings.TrimPrefix(assocKey.Type, "*")
	} else if strings.HasPrefix(assocKey.Type, "*") {
		foreignKeyType = assocKey.Type
	} else if strings.Contains(assocKey.Type, "[]byte") {
		foreignKeyType = assocKey.Type
	} else {
		foreignKeyType = "*" + assocKey.Type
	}

	foreignKey := &Field{Type: foreignKeyType, Package: assocKey.Package, GormFieldOptions: &gorm.GormFieldOptions{Tag: hasOne.GetForeignkeyTag()}}
	var foreignKeyName string
	if foreignKeyName = camelCase(hasOne.GetForeignkey()); foreignKeyName == "" {
		if b.countHasAssociationDimension(msg, fieldType) == 1 {
			foreignKeyName = fmt.Sprintf(typeName + assocKeyName)
		} else {
			foreignKeyName = fmt.Sprintf(fieldName + typeName + assocKeyName)
		}
	}

	hasOne.Foreignkey = foreignKeyName
	if _, ok := child.Fields[foreignKeyName]; child.Package != parent.Package && !ok {
		panic(fmt.Sprintf("Object %s from package %s cannot be user for has-one in %s since it does not have FK field %s defined. Manually define the key, or switch to belongs-to.",
			child.Name, child.Package, parent.Name, foreignKeyName))
	}
	if exField, ok := child.Fields[foreignKeyName]; !ok {
		child.Fields[foreignKeyName] = foreignKey
	} else {
		if exField.Type == "interface{}" {
			exField.Type = foreignKey.Type
		} else if !b.sameType(exField, foreignKey) {
			panic(fmt.Sprintf("Cannot include %s field into %s as it already exists there with a different type: %s, %s",
				foreignKeyName, child.Name, exField.Type, foreignKey.Type))
		}
	}

	child.Fields[foreignKeyName].ParentOrigName = parent.OriginName
}

func (b *ORMBuilder) parseHasMany(msg *protogen.Message, parent *OrmableType, fieldName string, fieldType string, child *OrmableType, opts *gorm.GormFieldOptions) {
	typeName := camelCase(string(msg.Desc.Name()))
	hasMany := opts.GetHasMany()
	if hasMany == nil {
		hasMany = &gorm.HasManyOptions{}
		opts.Association = &gorm.GormFieldOptions_HasMany{hasMany}
	}
	var assocKey *Field
	var assocKeyName string
	if assocKeyName = camelCase(hasMany.GetAssociationForeignkey()); assocKeyName == "" {
		assocKeyName, assocKey = b.findPrimaryKey(parent)
	} else {
		var ok bool
		assocKey, ok = parent.Fields[assocKeyName]
		if !ok {
			panic(fmt.Sprintf("Missing %s field in %s", assocKeyName, parent.Name))
		}
	}

	hasMany.AssociationForeignkey = assocKeyName
	var foreignKeyType string
	if hasMany.GetForeignkeyTag().GetNotNull() {
		foreignKeyType = strings.TrimPrefix(assocKey.Type, "*")
	} else if strings.HasPrefix(assocKey.Type, "*") {
		foreignKeyType = assocKey.Type
	} else if strings.Contains(assocKey.Type, "[]byte") {
		foreignKeyType = assocKey.Type
	} else {
		foreignKeyType = "*" + assocKey.Type
	}
	foreignKey := &Field{Type: foreignKeyType, Package: assocKey.Package, GormFieldOptions: &gorm.GormFieldOptions{Tag: hasMany.GetForeignkeyTag()}}
	var foreignKeyName string
	if foreignKeyName = hasMany.GetForeignkey(); foreignKeyName == "" {
		if b.countHasAssociationDimension(msg, fieldType) == 1 {
			foreignKeyName = fmt.Sprintf(typeName + assocKeyName)
		} else {
			foreignKeyName = fmt.Sprintf(fieldName + typeName + assocKeyName)
		}
	}
	hasMany.Foreignkey = foreignKeyName
	if _, ok := child.Fields[foreignKeyName]; child.Package != parent.Package && !ok {
		panic(fmt.Sprintf("Object %s from package %s cannot be user for has-many in %s since it does not have FK field %s defined. Manually define the key, or switch to many-to-many.",
			child.Name, child.Package, parent.Name, foreignKeyName))

	}
	if exField, ok := child.Fields[foreignKeyName]; !ok {
		child.Fields[foreignKeyName] = foreignKey
	} else {
		if exField.Type == "interface{}" {
			exField.Type = foreignKey.Type
		} else if !b.sameType(exField, foreignKey) {
			panic(fmt.Sprintf("Cannot include %s field into %s as it already exists there with a different type: %s, %s",
				foreignKeyName, child.Name, exField.Type, foreignKey.Type))
		}
	}
	child.Fields[foreignKeyName].ParentOrigName = parent.OriginName

	var posField string
	if posField = camelCase(hasMany.GetPositionField()); posField != "" {
		if exField, ok := child.Fields[posField]; !ok {
			child.Fields[posField] = &Field{Type: "int", GormFieldOptions: &gorm.GormFieldOptions{Tag: hasMany.GetPositionFieldTag()}}
		} else {
			if !strings.Contains(exField.Type, "int") {
				panic(fmt.Sprintf("Cannot include %s field into %s as it already exists there with a different type.",
					posField, child.Name))
			}
		}
		hasMany.PositionField = posField
	}
}

func (b *ORMBuilder) parseBelongsTo(msg *protogen.Message, child *OrmableType, fieldName string, fieldType string, parent *OrmableType, opts *gorm.GormFieldOptions) {
	belongsTo := opts.GetBelongsTo()
	if belongsTo == nil {
		belongsTo = &gorm.BelongsToOptions{}
		opts.Association = &gorm.GormFieldOptions_BelongsTo{belongsTo}
	}
	var assocKey *Field
	var assocKeyName string
	if assocKeyName = camelCase(belongsTo.GetAssociationForeignkey()); assocKeyName == "" {
		assocKeyName, assocKey = b.findPrimaryKey(parent)
	} else {
		var ok bool
		assocKey, ok = parent.Fields[assocKeyName]
		if !ok {
			panic(fmt.Sprintf("Missing %s field in %s", assocKeyName, parent.Name))
		}
	}
	belongsTo.AssociationForeignkey = assocKeyName
	var foreignKeyType string
	if belongsTo.GetForeignkeyTag().GetNotNull() {
		foreignKeyType = strings.TrimPrefix(assocKey.Type, "*")
	} else if strings.HasPrefix(assocKey.Type, "*") {
		foreignKeyType = assocKey.Type
	} else if strings.Contains(assocKey.Type, "[]byte") {
		foreignKeyType = assocKey.Type
	} else {
		foreignKeyType = "*" + assocKey.Type
	}
	foreignKey := &Field{Type: foreignKeyType, Package: assocKey.Package, GormFieldOptions: &gorm.GormFieldOptions{Tag: belongsTo.GetForeignkeyTag()}}
	var foreignKeyName string
	if foreignKeyName = camelCase(belongsTo.GetForeignkey()); foreignKeyName == "" {
		if b.countBelongsToAssociationDimension(msg, fieldType) == 1 {
			foreignKeyName = fmt.Sprintf(fieldType + assocKeyName)
		} else {
			foreignKeyName = fmt.Sprintf(fieldName + assocKeyName)
		}
	}
	belongsTo.Foreignkey = foreignKeyName
	if exField, ok := child.Fields[foreignKeyName]; !ok {
		child.Fields[foreignKeyName] = foreignKey
	} else {
		if exField.Type == "interface{}" {
			exField.Type = foreignKeyType
		} else if !b.sameType(exField, foreignKey) {
			panic(fmt.Sprintf("Cannot include %s field into %s as it already exists there with a different type: %s, %s",
				foreignKeyName, child.Name, exField.Type, foreignKey.Type))
		}
	}
	child.Fields[foreignKeyName].ParentOrigName = parent.OriginName
}

func (b *ORMBuilder) parseBasicFields(msg *protogen.Message, g *protogen.GeneratedFile) {
	typeName := string(msg.Desc.Name())
	ormable, ok := b.ormableTypes[typeName]
	if !ok {
		panic("typeName should be found")
	}
	ormable.Name = fmt.Sprintf("%sORM", typeName) // TODO: there are no reason to do it here

	for _, field := range msg.Fields {
		fd := field.Desc
		options := fd.Options().(*descriptorpb.FieldOptions)
		gormOptions := getFieldOptions(options)
		if gormOptions == nil {
			gormOptions = &gorm.GormFieldOptions{}
		}
		if gormOptions.GetDrop() {
			continue
		}

		tag := gormOptions.Tag
		fieldName := camelCase(string(fd.Name()))
		fieldType := fd.Kind().String()

		var typePackage string

		if b.dbEngine == ENGINE_POSTGRES && b.IsAbleToMakePQArray(fieldType) && field.Desc.IsList() {
			switch fieldType {
			case "bool":
				fieldType = generateImport("BoolArray", pqImport, g)
				gormOptions.Tag = tagWithType(tag, "bool[]")
			case "double":
				fieldType = generateImport("Float64Array", pqImport, g)
				gormOptions.Tag = tagWithType(tag, "float[]")
			case "int64":
				fieldType = generateImport("Int64Array", pqImport, g)
				gormOptions.Tag = tagWithType(tag, "integer[]")
			case "string":
				fieldType = generateImport("StringArray", pqImport, g)
				gormOptions.Tag = tagWithType(tag, "text[]")
			default:
				continue
			}
		} else if (field.Message == nil || !b.isOrmable(fieldType)) && field.Desc.IsList() {
			// not implemented
			continue
		} else if field.Enum != nil {
			fieldType = "int32"
			if b.stringEnums {
				fieldType = "string"
			}
		} else if field.Message != nil {
			xs := strings.Split(string(field.Message.Desc.FullName()), ".")
			rawType := xs[len(xs)-1]

			if v, ok := wellKnownTypes[rawType]; ok {
				fieldType = v
			} else if rawType == protoTypeBigInt {
				typePackage = bigintImport
				fieldType = "*" + generateImport("Int", bigintImport, g)
				if b.dbEngine == ENGINE_POSTGRES {
					gormOptions.Tag = tagWithType(tag, "numeric")
				}
			} else if rawType == protoTypeUUID {
				typePackage = uuidImport
				fieldType = generateImport("UUID", uuidImport, g)
				if b.dbEngine == ENGINE_POSTGRES {
					gormOptions.Tag = tagWithType(tag, "uuid")
				}
			} else if rawType == protoTypeUUIDValue {
				typePackage = uuidImport
				fieldType = "*" + generateImport("UUID", uuidImport, g)
				if b.dbEngine == ENGINE_POSTGRES {
					gormOptions.Tag = tagWithType(tag, "uuid")
				}
			} else if rawType == protoTypeTimestamp {
				typePackage = stdTimeImport
				fieldType = "*" + generateImport("Time", stdTimeImport, g)
			} else if rawType == protoTypeJSON {
				if b.dbEngine == ENGINE_POSTGRES {
					typePackage = gormpqImport
					fieldType = "*" + generateImport("Jsonb", gormpqImport, g)
					gormOptions.Tag = tagWithType(tag, "jsonb")
				} else {
					// Potential TODO: add types we want to use in other/default DB engine
					continue
				}
			} else if rawType == protoTypeResource {
				ttype := strings.ToLower(tag.GetType())
				if strings.Contains(ttype, "char") {
					ttype = "char"
				}
				if field.Desc.IsList() {
					ttype = "array"
				}
				switch ttype {
				case "uuid", "text", "char", "array", "cidr", "inet", "macaddr":
					fieldType = "*string"
				case "smallint", "integer", "bigint", "numeric", "smallserial", "serial", "bigserial":
					fieldType = "*int64"
				case "jsonb", "bytea":
					fieldType = "[]byte"
				case "":
					fieldType = "interface{}" // we do not know the type yet (if it association we will fix the type later)
				default:
					panic("unknown tag type of atlas.rpc.Identifier")
				}
				if tag.GetNotNull() || tag.GetPrimaryKey() {
					fieldType = strings.TrimPrefix(fieldType, "*")
				}
			} else if rawType == protoTypeInet {
				typePackage = gtypesImport
				fieldType = "*" + generateImport("Inet", gtypesImport, g)

				if b.dbEngine == ENGINE_POSTGRES {
					gormOptions.Tag = tagWithType(tag, "inet")
				} else {
					gormOptions.Tag = tagWithType(tag, "varchar(48)")
				}
			} else if rawType == protoTimeOnly {
				fieldType = "string"
				gormOptions.Tag = tagWithType(tag, "time")
			} else {
				continue
			}
		}

		switch fieldType {
		case "float":
			fieldType = "float32"
		case "double":
			fieldType = "float64"
		}

		f := &Field{
			GormFieldOptions: gormOptions,
			ParentGoType:     "",
			Type:             fieldType,
			Package:          typePackage,
		}

		if tName := gormOptions.GetReferenceOf(); tName != "" {
			if _, ok := b.messages[tName]; !ok {
				panic("unknow")
			}
			f.ParentOrigName = tName
		}

		ormable.Fields[fieldName] = f
	}

	gormMsgOptions := getMessageOptions(msg)

	// TODO: GetInclude
	for _, field := range gormMsgOptions.GetInclude() {
		fieldName := camelCase(field.GetName())
		if _, ok := ormable.Fields[fieldName]; !ok {
			b.addIncludedField(ormable, field, g)
		} else {
			panic("cound not include")
		}
	}
}

func (b *ORMBuilder) addIncludedField(ormable *OrmableType, field *gorm.ExtraField, g *protogen.GeneratedFile) {
	fieldName := camelCase(field.GetName())
	isPtr := strings.HasPrefix(field.GetType(), "*")
	rawType := strings.TrimPrefix(field.GetType(), "*")
	// cut off any package subpaths
	rawType = rawType[strings.LastIndex(rawType, ".")+1:]
	var typePackage string
	// Handle types with a package defined
	if field.GetPackage() != "" {
		rawType = generateImport(rawType, field.GetPackage(), g)
		typePackage = field.GetPackage()
	} else {
		// Handle types without a package defined
		if _, ok := builtinTypes[rawType]; ok {
			// basic type, 100% okay, no imports or changes needed
		} else if rawType == "Time" {
			// b.UsingGoImports(stdTimeImport) // TODO: missing UsingGoImports
			rawType = generateImport("Time", stdTimeImport, g)
		} else if rawType == "BigInt" {
			rawType = generateImport("Int", bigintImport, g)
		} else if rawType == "UUID" {
			rawType = generateImport("UUID", uuidImport, g)
		} else if field.GetType() == "Jsonb" && b.dbEngine == ENGINE_POSTGRES {
			rawType = generateImport("Jsonb", gormpqImport, g)
		} else if rawType == "Inet" {
			rawType = generateImport("Inet", gtypesImport, g)
		} else {
			fmt.Fprintf(os.Stderr, "included field %q of type %q is not a recognized special type, and no package specified. This type is assumed to be in the same package as the generated code",
				field.GetName(), field.GetType())
		}
	}
	if isPtr {
		rawType = fmt.Sprintf("*%s", rawType)
	}
	ormable.Fields[fieldName] = &Field{Type: rawType, Package: typePackage, GormFieldOptions: &gorm.GormFieldOptions{Tag: field.GetTag()}}
}

func getFieldOptions(options *descriptorpb.FieldOptions) *gorm.GormFieldOptions {
	if options == nil {
		return nil
	}

	v := proto.GetExtension(options, gorm.E_Field)
	if v == nil {
		return nil
	}

	opts, ok := v.(*gorm.GormFieldOptions)
	if !ok {
		return nil
	}

	return opts
}

// retrieves the GormMessageOptions from a message
func getMessageOptions(message *protogen.Message) *gorm.GormMessageOptions {
	options := message.Desc.Options()
	if options == nil {
		return nil
	}
	v := proto.GetExtension(options, gorm.E_Opts)
	if v == nil {
		return nil
	}

	opts, ok := v.(*gorm.GormMessageOptions)
	if !ok {
		return nil
	}

	return opts
}

func isOrmable(message *protogen.Message) bool {
	desc := message.Desc
	options := desc.Options()

	m, ok := proto.GetExtension(options, gorm.E_Opts).(*gorm.GormMessageOptions)
	if !ok || m == nil {
		return false
	}

	return m.Ormable
}

func (b *ORMBuilder) IsAbleToMakePQArray(fieldType string) bool {
	switch fieldType {
	case "bool", "double", "int64", "string":
		return true
	default:
		return false
	}
}

func tagWithType(tag *gorm.GormTag, typename string) *gorm.GormTag {
	if tag == nil {
		tag = &gorm.GormTag{}
	}

	tag.Type = typename
	return tag
}

func camelCase(s string) string {
	if s == "" {
		return ""
	}
	t := make([]byte, 0, 32)
	i := 0
	if s[0] == '_' {
		// Need a capital letter; drop the '_'.
		t = append(t, 'X')
		i++
	}
	// Invariant: if the next letter is lower case, it must be converted
	// to upper case.
	// That is, we process a word at a time, where words are marked by _ or
	// upper case letter. Digits are treated as words.
	for ; i < len(s); i++ {
		c := s[i]
		if c == '_' && i+1 < len(s) && isASCIILower(s[i+1]) {
			continue // Skip the underscore in s.
		}
		if isASCIIDigit(c) {
			t = append(t, c)
			continue
		}
		// Assume we have a letter now - if not, it's a bogus identifier.
		// The next word is a sequence of characters that must start upper case.
		if isASCIILower(c) {
			c ^= ' ' // Make it a capital letter.
		}
		t = append(t, c) // Guaranteed not lower case.
		// Accept lower case sequence that follows.
		for i+1 < len(s) && isASCIILower(s[i+1]) {
			i++
			t = append(t, s[i])
		}
	}
	return string(t)
}

func isASCIILower(c byte) bool {
	return 'a' <= c && c <= 'z'
}

func isASCIIDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

func (b *ORMBuilder) renderGormTag(field *Field) string {
	var gormRes, atlasRes string
	tag := field.GetTag()
	if tag == nil {
		tag = &gorm.GormTag{}
	}

	if len(tag.Column) > 0 {
		gormRes += fmt.Sprintf("column:%s;", tag.GetColumn())
	}
	if len(tag.Type) > 0 {
		gormRes += fmt.Sprintf("type:%s;", tag.GetType())
	}
	if tag.GetSize() > 0 {
		gormRes += fmt.Sprintf("size:%d;", tag.GetSize())
	}
	if tag.Precision > 0 {
		gormRes += fmt.Sprintf("precision:%d;", tag.GetPrecision())
	}
	if tag.GetPrimaryKey() {
		gormRes += "primary_key;"
	}
	if tag.GetUnique() {
		gormRes += "unique;"
	}
	if len(tag.Default) > 0 {
		gormRes += fmt.Sprintf("default:%s;", tag.GetDefault())
	}
	if tag.GetNotNull() {
		gormRes += "not null;"
	}
	if tag.GetAutoIncrement() {
		gormRes += "auto_increment;"
	}
	if len(tag.Index) > 0 {
		if tag.GetIndex() == "" {
			gormRes += "index;"
		} else {
			gormRes += fmt.Sprintf("index:%s;", tag.GetIndex())
		}
	}
	if len(tag.UniqueIndex) > 0 {
		if tag.GetUniqueIndex() == "" {
			gormRes += "unique_index;"
		} else {
			gormRes += fmt.Sprintf("unique_index:%s;", tag.GetUniqueIndex())
		}
	}
	if tag.GetEmbedded() {
		gormRes += "embedded;"
	}
	if len(tag.EmbeddedPrefix) > 0 {
		gormRes += fmt.Sprintf("embedded_prefix:%s;", tag.GetEmbeddedPrefix())
	}
	if tag.GetIgnore() {
		gormRes += "-;"
	}

	var foreignKey, associationForeignKey, joinTable, joinTableForeignKey, associationJoinTableForeignKey string
	var associationAutoupdate, associationAutocreate, associationSaveReference, preload, replace, append, clear bool
	if hasOne := field.GetHasOne(); hasOne != nil {
		foreignKey = hasOne.Foreignkey
		associationForeignKey = hasOne.AssociationForeignkey
		associationAutoupdate = hasOne.AssociationAutoupdate
		associationAutocreate = hasOne.AssociationAutocreate
		associationSaveReference = hasOne.AssociationSaveReference
		preload = hasOne.Preload
		clear = hasOne.Clear
		replace = hasOne.Replace
		append = hasOne.Append
	} else if belongsTo := field.GetBelongsTo(); belongsTo != nil {
		foreignKey = belongsTo.Foreignkey
		associationForeignKey = belongsTo.AssociationForeignkey
		associationAutoupdate = belongsTo.AssociationAutoupdate
		associationAutocreate = belongsTo.AssociationAutocreate
		associationSaveReference = belongsTo.AssociationSaveReference
		preload = belongsTo.Preload
	} else if hasMany := field.GetHasMany(); hasMany != nil {
		foreignKey = hasMany.Foreignkey
		associationForeignKey = hasMany.AssociationForeignkey
		associationAutoupdate = hasMany.AssociationAutoupdate
		associationAutocreate = hasMany.AssociationAutocreate
		associationSaveReference = hasMany.AssociationSaveReference
		clear = hasMany.Clear
		preload = hasMany.Preload
		replace = hasMany.Replace
		append = hasMany.Append
		if len(hasMany.PositionField) > 0 {
			atlasRes += fmt.Sprintf("position:%s;", hasMany.GetPositionField())
		}
	} else if mtm := field.GetManyToMany(); mtm != nil {
		foreignKey = mtm.Foreignkey
		associationForeignKey = mtm.AssociationForeignkey
		joinTable = mtm.Jointable
		joinTableForeignKey = mtm.JointableForeignkey
		associationJoinTableForeignKey = mtm.AssociationJointableForeignkey
		associationAutoupdate = mtm.AssociationAutoupdate
		associationAutocreate = mtm.AssociationAutocreate
		associationSaveReference = mtm.AssociationSaveReference
		preload = mtm.Preload
		clear = mtm.Clear
		replace = mtm.Replace
		append = mtm.Append
	} else {
		foreignKey = tag.Foreignkey
		associationForeignKey = tag.AssociationForeignkey
		joinTable = tag.ManyToMany
		joinTableForeignKey = tag.JointableForeignkey
		associationJoinTableForeignKey = tag.AssociationJointableForeignkey
		associationAutoupdate = tag.AssociationAutoupdate
		associationAutocreate = tag.AssociationAutocreate
		associationSaveReference = tag.AssociationSaveReference
		preload = tag.Preload
	}

	if len(foreignKey) > 0 {
		gormRes += fmt.Sprintf("foreignkey:%s;", foreignKey)
	}

	if len(associationForeignKey) > 0 {
		gormRes += fmt.Sprintf("association_foreignkey:%s;", associationForeignKey)
	}

	if len(joinTable) > 0 {
		gormRes += fmt.Sprintf("many2many:%s;", joinTable)
	}
	if len(joinTableForeignKey) > 0 {
		gormRes += fmt.Sprintf("jointable_foreignkey:%s;", joinTableForeignKey)
	}
	if len(associationJoinTableForeignKey) > 0 {
		gormRes += fmt.Sprintf("association_jointable_foreignkey:%s;", associationJoinTableForeignKey)
	}

	if associationAutoupdate {
		gormRes += fmt.Sprintf("association_autoupdate:%s;", strconv.FormatBool(associationAutoupdate))
	}

	if associationAutocreate {
		gormRes += fmt.Sprintf("association_autocreate:%s;", strconv.FormatBool(associationAutocreate))
	}

	if associationSaveReference {
		gormRes += fmt.Sprintf("association_save_reference:%s;", strconv.FormatBool(associationSaveReference))
	}

	if preload {
		gormRes += fmt.Sprintf("preload:%s;", strconv.FormatBool(preload))
	}

	if clear {
		gormRes += fmt.Sprintf("clear:%s;", strconv.FormatBool(clear))
	} else if replace {
		gormRes += fmt.Sprintf("replace:%s;", strconv.FormatBool(replace))
	} else if append {
		gormRes += fmt.Sprintf("append:%s;", strconv.FormatBool(append))
	}

	var gormTag, atlasTag string
	if gormRes != "" {
		gormTag = fmt.Sprintf("gorm:\"%s\"", strings.TrimRight(gormRes, ";"))
	}
	if atlasRes != "" {
		atlasTag = fmt.Sprintf("atlas:\"%s\"", strings.TrimRight(atlasRes, ";"))
	}
	finalTag := strings.TrimSpace(strings.Join([]string{gormTag, atlasTag}, " "))
	if finalTag == "" {
		return ""
	} else {
		return fmt.Sprintf("`%s`", finalTag)
	}
}

func (b *ORMBuilder) setupOrderedHasMany(message *protogen.Message, g *protogen.GeneratedFile) {
	typeName := string(message.Desc.Name())
	ormable := b.getOrmable(typeName)
	var fieldNames []string
	for name := range ormable.Fields {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)

	for _, fieldName := range fieldNames {
		b.setupOrderedHasManyByName(message, fieldName, g)
	}
}

func (b *ORMBuilder) setupOrderedHasManyByName(message *protogen.Message, fieldName string, g *protogen.GeneratedFile) {
	typeName := string(message.Desc.Name())
	ormable := b.getOrmable(typeName)
	field := ormable.Fields[fieldName]

	if field == nil {
		return
	}

	if field.GetHasMany().GetPositionField() != "" {
		positionField := field.GetHasMany().GetPositionField()
		positionFieldType := b.getOrmable(field.Type).Fields[positionField].Type
		g.P(`for i, e := range `, `to.`, fieldName, `{`)
		g.P(`e.`, positionField, ` = `, positionFieldType, `(i)`)
		g.P(`}`)
	}
}

// Output code that will convert a field to/from orm.
func (b *ORMBuilder) generateFieldConversion(message *protogen.Message, field *protogen.Field,
	toORM bool, ofield *Field, g *protogen.GeneratedFile) error {

	fieldName := camelCase(string(field.Desc.Name()))
	fieldType := field.Desc.Kind().String() // was GoType
	if field.Desc.Message() != nil {
		parts := strings.Split(string(field.Desc.Message().FullName()), ".")
		fieldType = parts[len(parts)-1]
	}
	if field.Desc.Cardinality() == protoreflect.Repeated {
		// Some repeated fields can be handled by github.com/lib/pq
		if b.dbEngine == ENGINE_POSTGRES && b.IsAbleToMakePQArray(fieldType) && field.Desc.IsList() {
			g.P(`if m.`, fieldName, ` != nil {`)
			switch fieldType {
			case "bool":
				g.P(`to.`, fieldName, ` = make(`, generateImport("BoolArray", pqImport, g), `, len(m.`, fieldName, `))`)
			case "double":
				g.P(`to.`, fieldName, ` = make(`, generateImport("Float64Array", pqImport, g), `, len(m.`, fieldName, `))`)
			case "int64":
				g.P(`to.`, fieldName, ` = make(`, generateImport("Int64Array", pqImport, g), `, len(m.`, fieldName, `))`)
			case "string":
				g.P(`to.`, fieldName, ` = make(`, generateImport("StringArray", pqImport, g), `, len(m.`, fieldName, `))`)
			}
			g.P(`copy(to.`, fieldName, `, m.`, fieldName, `)`)
			g.P(`}`)
		} else if b.isOrmable(fieldType) { // Repeated ORMable type
			//fieldType = strings.Trim(fieldType, "[]*")

			g.P(`for _, v := range m.`, fieldName, ` {`)
			g.P(`if v != nil {`)
			if toORM {
				g.P(`if temp`, fieldName, `, cErr := v.ToORM(ctx); cErr == nil {`)
			} else {
				g.P(`if temp`, fieldName, `, cErr := v.ToPB(ctx); cErr == nil {`)
			}
			g.P(`to.`, fieldName, ` = append(to.`, fieldName, `, &temp`, fieldName, `)`)
			g.P(`} else {`)
			g.P(`return to, cErr`)
			g.P(`}`)
			g.P(`} else {`)
			g.P(`to.`, fieldName, ` = append(to.`, fieldName, `, nil)`)
			g.P(`}`)
			g.P(`}`) // end repeated for
		} else {
			g.P(`// Repeated type `, fieldType, ` is not an ORMable message type`)
		}
	} else if field.Enum != nil { // Singular Enum, which is an int32 ---
		fieldType = b.typeName(field.Enum.GoIdent, g)
		if toORM {
			if b.stringEnums {
				g.P(`to.`, fieldName, ` = `, fieldType, `_name[int32(m.`, fieldName, `)]`)
			} else {
				g.P(`to.`, fieldName, ` = int32(m.`, fieldName, `)`)
			}
		} else {
			if b.stringEnums {
				g.P(`to.`, fieldName, ` = `, fieldType, `(`, fieldType, `_value[m.`, fieldName, `])`)
			} else {
				g.P(`to.`, fieldName, ` = `, fieldType, `(m.`, fieldName, `)`)
			}
		}
	} else if field.Message != nil { // Singular Object -------------
		//Check for WKTs
		// Type is a WKT, convert to/from as ptr to base type
		if _, exists := wellKnownTypes[fieldType]; exists { // Singular WKT -----
			if toORM {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`v := m.`, fieldName, `.Value`)
				g.P(`to.`, fieldName, ` = &v`)
				g.P(`}`)
			} else {
				g.P(`if m.`, fieldName, ` != nil {`)
				// g.P(`to.`, fieldName, ` = &`, b.GetFileImports().wktPkgName, ".", fieldType,
				// 	`{Value: *m.`, fieldName, `}`)
				g.P(`to.`, fieldName, ` = &`, generateImport(fieldType, wktImport, g),
					`{Value: *m.`, fieldName, `}`)
				g.P(`}`)
			}
		} else if fieldType == protoTypeBigInt { // Singular BigInt type ----
			if toORM {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`var ok bool`)
				g.P(`to.`, fieldName, ` = new(big.Int)`)
				g.P(`to.`, fieldName, `, ok = to.`, fieldName, `.SetString(m.`, fieldName, `.Value, 0)`)
				g.P(`if !ok {`)
				g.P(`return to, fmt.Errorf("unable convert `, fieldName, ` to big.Int")`)
				g.P(`}`)
				g.P(`}`)
			} else {
				g.P(`to.`, fieldName, ` = &`, generateImport("BigInt", gtypesImport, g), `{Value: m.`, fieldName, `.String()}`)
			}
		} else if fieldType == protoTypeUUIDValue { // Singular UUIDValue type ----
			if toORM {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`tempUUID, uErr := `, generateImport("FromString", uuidImport, g), `(m.`, fieldName, `.Value)`)
				g.P(`if uErr != nil {`)
				g.P(`return to, uErr`)
				g.P(`}`)
				g.P(`to.`, fieldName, ` = &tempUUID`)
				g.P(`}`)
			} else {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`to.`, fieldName, ` = &`, generateImport("UUIDValue", gtypesImport, g), `{Value: m.`, fieldName, `.String()}`)
				g.P(`}`)
			}
		} else if fieldType == protoTypeUUID { // Singular UUID type --------------
			if toORM {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`to.`, fieldName, `, err = `, generateImport("FromString", uuidImport, g), `(m.`, fieldName, `.Value)`)
				g.P(`if err != nil {`)
				g.P(`return to, err`)
				g.P(`}`)
				g.P(`} else {`)
				g.P(`to.`, fieldName, ` = `, generateImport("Nil", uuidImport, g))
				g.P(`}`)
			} else {
				g.P(`to.`, fieldName, ` = &`, generateImport("UUID", gtypesImport, g), `{Value: m.`, fieldName, `.String()}`)
			}
		} else if fieldType == protoTypeTimestamp { // Singular WKT Timestamp ---
			if toORM {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`t := m.`, fieldName, `.AsTime()`)
				g.P(`to.`, fieldName, ` = &t`)
				g.P(`}`)
			} else {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`to.`, fieldName, ` = `, generateImport("New", timestampImport, g), `(*m.`, fieldName, `)`)
				g.P(`}`)
			}
		} else if fieldType == protoTypeJSON {
			if b.dbEngine == ENGINE_POSTGRES {
				if toORM {
					g.P(`if m.`, fieldName, ` != nil {`)
					g.P(`to.`, fieldName, ` = &`, generateImport("Jsonb", gormpqImport, g), `{[]byte(m.`, fieldName, `.Value)}`)
					g.P(`}`)
				} else {
					g.P(`if m.`, fieldName, ` != nil {`)
					g.P(`to.`, fieldName, ` = &`, generateImport("JSONValue", gtypesImport, g), `{Value: string(m.`, fieldName, `.RawMessage)}`)
					g.P(`}`)
				}
			} // Potential TODO other DB engine handling if desired
		} else if fieldType == protoTypeResource {
			resource := "nil" // assuming we do not know the PB type, nil means call codec for any resource
			if ofield != nil && ofield.ParentOrigName != "" {
				resource = "&" + ofield.ParentOrigName + "{}"
			}
			btype := strings.TrimPrefix(ofield.Type, "*")
			nillable := strings.HasPrefix(ofield.Type, "*")
			iface := ofield.Type == "interface{}"

			if toORM {
				if nillable {
					g.P(`if m.`, fieldName, ` != nil {`)
				}
				switch btype {
				case "int64":
					g.P(`if v, err :=`, generateImport("DecodeInt64", resourceImport, g), `(`, resource, `, m.`, fieldName, `); err != nil {`)
					g.P(`	return to, err`)
					g.P(`} else {`)
					if nillable {
						g.P(`to.`, fieldName, ` = &v`)
					} else {
						g.P(`to.`, fieldName, ` = v`)
					}
					g.P(`}`)
				case "[]byte":
					g.P(`if v, err :=`, generateImport("DecodeBytes", resourceImport, g), `(`, resource, `, m.`, fieldName, `); err != nil {`)
					g.P(`	return to, err`)
					g.P(`} else {`)
					g.P(`	to.`, fieldName, ` = v`)
					g.P(`}`)
				default:
					g.P(`if v, err :=`, generateImport("Decode", resourceImport, g), `(`, resource, `, m.`, fieldName, `); err != nil {`)
					g.P(`return to, err`)
					g.P(`} else if v != nil {`)
					if nillable {
						g.P(`vv := v.(`, btype, `)`)
						g.P(`to.`, fieldName, ` = &vv`)
					} else if iface {
						g.P(`to.`, fieldName, `= v`)
					} else {
						g.P(`to.`, fieldName, ` = v.(`, btype, `)`)
					}
					g.P(`}`)
				}
				if nillable {
					g.P(`}`)
				}
			}

			// if !toORM {
			// 	if nillable {
			// 		g.P(`if m.`, fieldName, `!= nil {`)
			// 		g.P(`	if v, err := `, generateImport("Encode", resourceImport, g), `(`, resource, `, *m.`, fieldName, `); err != nil {`)
			// 		g.P(`		return to, err`)
			// 		g.P(`	} else {`)
			// 		g.P(`		to.`, fieldName, ` = v`)
			// 		g.P(`	}`)
			// 		g.P(`}`)

			// 	} else {
			// 		g.P(`if v, err := `, generateImport("Encode", resourceImport, g), `(`, resource, `, m.`, fieldName, `); err != nil {`)
			// 		g.P(`return to, err`)
			// 		g.P(`} else {`)
			// 		g.P(`to.`, fieldName, ` = v`)
			// 		g.P(`}`)
			// 	}
			// }
		} else if fieldType == protoTypeInet { // Inet type for Postgres only, currently
			if toORM {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`if to.`, fieldName, `, err = `, generateImport("ParseInet", gtypesImport, g), `(m.`, fieldName, `.Value); err != nil {`)
				g.P(`return to, err`)
				g.P(`}`)
				g.P(`}`)
			} else {
				g.P(`if m.`, fieldName, ` != nil && m.`, fieldName, `.IPNet != nil {`)
				g.P(`to.`, fieldName, ` = &`, generateImport("InetValue", gtypesImport, g), `{Value: m.`, fieldName, `.String()}`)
				g.P(`}`)
			}
		} else if fieldType == protoTimeOnly { // Time only to support time via string
			if toORM {
				g.P(`if m.`, fieldName, ` != nil {`)
				g.P(`if to.`, fieldName, `, err = `, generateImport("ParseTime", gtypesImport, g), `(m.`, fieldName, `.Value); err != nil {`)
				g.P(`return to, err`)
				g.P(`}`)
				g.P(`}`)
			} else {
				g.P(`if m.`, fieldName, ` != "" {`)
				g.P(`if to.`, fieldName, `, err = `, generateImport("TimeOnlyByString", gtypesImport, g), `( m.`, fieldName, `); err != nil {`)
				g.P(`return to, err`)
				g.P(`}`)
				g.P(`}`)
			}
		} else if b.isOrmable(fieldType) {
			// Not a WKT, but a type we're building converters for
			g.P(`if m.`, fieldName, ` != nil {`)
			if toORM {
				g.P(`temp`, fieldName, `, err := m.`, fieldName, `.ToORM (ctx)`)
			} else {
				g.P(`temp`, fieldName, `, err := m.`, fieldName, `.ToPB (ctx)`)
			}
			g.P(`if err != nil {`)
			g.P(`return to, err`)
			g.P(`}`)
			g.P(`to.`, fieldName, ` = &temp`, fieldName)
			g.P(`}`)
		}
	} else { // Singular raw ----------------------------------------------------
		g.P(`to.`, fieldName, ` = m.`, fieldName)
	}
	return nil
}

func (b *ORMBuilder) generateBeforeHookCall(orm *OrmableType, method string, g *protogen.GeneratedFile) {
	g.P(`if hook, ok := interface{}(&ormObj).(`, orm.Name, `WithBefore`, method, `); ok {`)
	g.P(`if db, err = hook.Before`, method, `(ctx, db); err != nil {`)
	g.P(`return nil, err`)
	g.P(`}`)
	g.P(`}`)
}

func (b *ORMBuilder) generateAfterHookCall(orm *OrmableType, method string, g *protogen.GeneratedFile) {
	g.P(`if hook, ok := interface{}(&ormObj).(`, orm.Name, `WithAfter`, method, `); ok {`)
	g.P(`if err = hook.After`, method, `(ctx, db); err != nil {`)
	g.P(`return nil, err`)
	g.P(`}`)
	g.P(`}`)
}

func (b *ORMBuilder) generateHookInterfaces(g *protogen.GeneratedFile, message *protogen.Message) {
	typeName := string(message.Desc.Name())
	g.P(`// The following are interfaces you can implement for special behavior during ORM/PB conversions`)
	g.P(`// of type `, typeName, ` the arg will be the target, the caller the one being converted from`)
	g.P()
	for _, desc := range [][]string{
		{"BeforeToORM", typeName + "ORM", " called before default ToORM code"},
		{"AfterToORM", typeName + "ORM", " called after default ToORM code"},
		{"BeforeToPB", typeName, " called before default ToPB code"},
		{"AfterToPB", typeName, " called after default ToPB code"},
	} {
		g.P(`// `, typeName, desc[0], desc[2])
		g.P(`type `, typeName, `With`, desc[0], ` interface {`)
		g.P(desc[0], `(context.Context, *`, desc[1], `) error`)
		g.P(`}`)
		g.P()
	}
}

func generateImport(name string, importPath string, g *protogen.GeneratedFile) string {
	return g.QualifiedGoIdent(protogen.GoIdent{
		GoName:       name,
		GoImportPath: protogen.GoImportPath(importPath),
	})
}

func (b *ORMBuilder) typeName(ident protogen.GoIdent, g *protogen.GeneratedFile) string {
	// drop package prefix, no need to import
	if b.currentPackage == ident.GoImportPath.String() {
		return ident.GoName
	}

	return generateImport(ident.GoName, string(ident.GoImportPath), g)
}

func GetOrmable(ormableTypes map[string]*OrmableType, typeName string) (*OrmableType, error) {
	parts := strings.Split(typeName, ".")
	ormable, ok := ormableTypes[strings.TrimSuffix(strings.Trim(parts[len(parts)-1], "[]*"), "ORM")]
	var err error
	if !ok {
		err = ErrNotOrmable
	}
	return ormable, err
}

func (b *ORMBuilder) countHasAssociationDimension(message *protogen.Message, typeName string) int {
	dim := 0
	for _, field := range message.Fields {
		options := field.Desc.Options().(*descriptorpb.FieldOptions)
		fieldOpts := getFieldOptions(options)
		if fieldOpts.GetDrop() {
			continue
		}

		var fieldType string
		if field.Desc.Message() == nil {
			fieldType = field.Desc.Kind().String() // was GoType
		} else {
			fieldType = string(field.Desc.Message().Name())
		}

		if fieldOpts.GetManyToMany() == nil && fieldOpts.GetBelongsTo() == nil {
			if strings.Trim(typeName, "[]*") == strings.Trim(fieldType, "[]*") {
				dim++
			}
		}
	}

	return dim
}

func (b *ORMBuilder) countBelongsToAssociationDimension(message *protogen.Message, typeName string) int {
	dim := 0
	for _, field := range message.Fields {
		options := field.Desc.Options().(*descriptorpb.FieldOptions)
		fieldOpts := getFieldOptions(options)
		if fieldOpts.GetDrop() {
			continue
		}

		var fieldType string
		if field.Desc.Message() == nil {
			fieldType = field.Desc.Kind().String() // was GoType
		} else {
			fieldType = string(field.Desc.Message().Name())
		}

		if fieldOpts.GetBelongsTo() != nil {
			if strings.Trim(typeName, "[]*") == strings.Trim(fieldType, "[]*") {
				dim++
			}
		}
	}

	return dim
}

func (b *ORMBuilder) countManyToManyAssociationDimension(message *protogen.Message, typeName string) int {
	dim := 0

	for _, field := range message.Fields {
		options := field.Desc.Options().(*descriptorpb.FieldOptions)
		fieldOpts := getFieldOptions(options)

		if fieldOpts.GetDrop() {
			continue
		}
		var fieldType string

		if field.Desc.Message() == nil {
			fieldType = field.Desc.Kind().String() // was GoType
		} else {
			fieldType = string(field.Desc.Message().Name())
		}

		if fieldOpts.GetManyToMany() != nil {
			if strings.Trim(typeName, "[]*") == strings.Trim(fieldType, "[]*") {
				dim++
			}
		}
	}

	return dim
}

func (b *ORMBuilder) sameType(field1 *Field, field2 *Field) bool {
	isPointer1 := strings.HasPrefix(field1.Type, "*")
	typeParts1 := strings.Split(field1.Type, ".")

	if len(typeParts1) == 2 {
		isPointer2 := strings.HasPrefix(field2.Type, "*")
		typeParts2 := strings.Split(field2.Type, ".")

		if len(typeParts2) == 2 && isPointer1 == isPointer2 && typeParts1[1] == typeParts2[1] && field1.Package == field2.Package {
			return true
		}

		return false
	}

	return field1.Type == field2.Type
}

func getFieldType(field *protogen.Field) string {
	if field.Desc.Message() == nil {
		return field.Desc.Kind().String()
	}

	return string(field.Desc.Message().Name())
}

func getFieldIdent(field *protogen.Field) protogen.GoIdent {
	if field.Desc.Message() != nil {
		return field.Message.GoIdent
	}

	return field.GoIdent
}
